/***** BEGIN LICENSE BLOCK *****
# This Source Code Form is subject to the terms of the Mozilla Public
# License, v. 2.0. If a copy of the MPL was not distributed with this file,
# You can obtain one at http://mozilla.org/MPL/2.0/.
#
# The Initial Developer of the Original Code is the Mozilla Foundation.
# Portions created by the Initial Developer are Copyright (C) 2012-2014
# the Initial Developer. All Rights Reserved.
#
# Contributor(s):
#   Rob Miller (rmiller@mozilla.com)
#   Mike Trinkala (trink@mozilla.com)
#   Justin Judd (justin@justinjudd.org)
#
# ***** END LICENSE BLOCK *****/

package pipeline

import (
	"bufio"
	"bytes"
	"code.google.com/p/go-uuid/uuid"
	"errors"
	"fmt"
	"github.com/bbangert/toml"
	"io"
	"io/ioutil"
	"log"
	"os"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	HEKA_DAEMON     = "hekad"
	invalidEnvChars = "\n\r\t "
)

var (
	invalidEnvPrefix     = []byte("%ENV[")
	AvailablePlugins     = make(map[string]func() interface{})
	ErrMissingCloseDelim = errors.New("Missing closing delimiter")
	ErrInvalidChars      = errors.New("Invalid characters in environmental variable")
)

// Adds a plugin to the set of usable Heka plugins that can be referenced from
// a Heka config file.
func RegisterPlugin(name string, factory func() interface{}) {
	AvailablePlugins[name] = factory
}

// Generic plugin configuration type that will be used for plugins that don't
// provide the `HasConfigStruct` interface.
type PluginConfig map[string]toml.Primitive

// API made available to all plugins providing Heka-wide utility functions.
type PluginHelper interface {

	// Returns an `OutputRunner` for an output plugin registered using the
	// specified name, or ok == false if no output by that name is registered.
	Output(name string) (oRunner OutputRunner, ok bool)

	// Returns the running `FilterRunner` for a filter plugin registered using
	// the specified name, or ok == false if no filter by that name is
	// registered.
	Filter(name string) (fRunner FilterRunner, ok bool)

	// Instantiates and returns an `Encoder` plugin of the specified name, or
	// ok == false if no encoder by that name is registered.
	Encoder(base_name, full_name string) (encoder Encoder, ok bool)

	// Returns the currently running Heka instance's unique PipelineConfig
	// object.
	PipelineConfig() *PipelineConfig

	// Instantiates, starts, and returns a DecoderRunner wrapped around a newly
	// created Decoder of the specified name.
	DecoderRunner(base_name, full_name string) (dRunner DecoderRunner, ok bool)

	// Stops and unregisters the provided DecoderRunner.
	StopDecoderRunner(dRunner DecoderRunner) (ok bool)

	// Expects a loop count value from an existing message (or zero if there's
	// no relevant existing message), returns an initialized `PipelinePack`
	// pointer that can be populated w/ message data and inserted into the
	// Heka pipeline. Returns `nil` if the loop count value provided is
	// greater than the maximum allowed by the Heka instance.
	PipelinePack(msgLoopCount uint) *PipelinePack

	// Returns an input plugin of the given name that provides the
	// StatAccumulator interface, or an error value if such a plugin
	// can't be found.
	StatAccumulator(name string) (statAccum StatAccumulator, err error)

	// Returns the configured Hostname for the Heka process. This can come
	// either from the runtime or from the Heka config.
	Hostname() string
}

// Indicates a plug-in has a specific-to-itself config struct that should be
// passed in to its Init method.
type HasConfigStruct interface {
	// Returns a default-value-populated configuration structure into which
	// the plugin's TOML configuration will be deserialized.
	ConfigStruct() interface{}
}

// Master config object encapsulating the entire heka/pipeline configuration.
type PipelineConfig struct {
	// Heka global values.
	Globals *GlobalConfigStruct
	// PluginMakers for every registered plugin, by category.
	makers map[string]map[string]PluginMaker
	// Direct access to makers["Decoder"] since it's needed by MultiDecoder
	// outside of the pipeline package.
	DecoderMakers map[string]PluginMaker
	// Mutex protecting the makers map.
	makersLock sync.RWMutex
	// All running InputRunners, by name.
	InputRunners map[string]InputRunner
	// All running FilterRunners, by name.
	FilterRunners map[string]FilterRunner
	// All running OutputRunners, by name.
	OutputRunners map[string]OutputRunner
	// Heka message router instance.
	router *messageRouter
	// PipelinePack supply for Input plugins.
	inputRecycleChan chan *PipelinePack
	// PipelinePack supply for Filter plugins (separate pool prevents
	// deadlocks).
	injectRecycleChan chan *PipelinePack
	// Stores log messages generated by plugin config errors.
	LogMsgs []string
	// Lock protecting access to the set of running filters so dynamic filters
	// can be safely added and removed while Heka is running.
	filtersLock sync.RWMutex
	// Is freed when all FilterRunners have stopped.
	filtersWg sync.WaitGroup
	// Is freed when all DecoderRunners have stopped.
	decodersWg sync.WaitGroup
	// Slice providing access to all running DecoderRunners.
	allDecoders []DecoderRunner
	// Mutex protecting allDecoders.
	allDecodersLock sync.RWMutex
	// Slice providing access to all instantiated Encoders.
	allEncoders map[string]Encoder
	// Mutex protecting allEncoders.
	allEncodersLock sync.RWMutex
	// Name of host on which Heka is running.
	hostname string
	// Heka process id.
	pid int32
	// Lock protecting access to the set of running inputs so they
	// can be safely added while Heka is running.
	inputsLock sync.RWMutex
	// Is freed when all Input runners have stopped.
	inputsWg sync.WaitGroup
	// Lock protecting access to running outputs so they can be removed
	// safely.
	outputsLock sync.RWMutex
	// Internal reporting channel.
	reportRecycleChan chan *PipelinePack
}

// Creates and initializes a PipelineConfig object. `nil` value for `globals`
// argument means we should use the default global config values.
func NewPipelineConfig(globals *GlobalConfigStruct) (config *PipelineConfig) {
	config = new(PipelineConfig)
	if globals == nil {
		globals = DefaultGlobals()
	}
	config.Globals = globals
	config.makers = make(map[string]map[string]PluginMaker)
	config.makers["Input"] = make(map[string]PluginMaker)
	config.makers["Decoder"] = make(map[string]PluginMaker)
	config.makers["Filter"] = make(map[string]PluginMaker)
	config.makers["Encoder"] = make(map[string]PluginMaker)
	config.makers["Output"] = make(map[string]PluginMaker)
	config.DecoderMakers = config.makers["Decoder"]

	config.InputRunners = make(map[string]InputRunner)
	config.FilterRunners = make(map[string]FilterRunner)
	config.OutputRunners = make(map[string]OutputRunner)

	config.allEncoders = make(map[string]Encoder)
	config.router = NewMessageRouter(globals.PluginChanSize)
	config.inputRecycleChan = make(chan *PipelinePack, globals.PoolSize)
	config.injectRecycleChan = make(chan *PipelinePack, globals.PoolSize)
	config.LogMsgs = make([]string, 0, 4)
	config.allDecoders = make([]DecoderRunner, 0, 10)
	config.hostname = globals.Hostname
	config.pid = int32(os.Getpid())
	config.reportRecycleChan = make(chan *PipelinePack, 1)

	return config
}

// Callers should pass in the msgLoopCount value from any relevant Message
// objects they are holding. Returns a PipelinePack for injection into Heka
// pipeline, or nil if the msgLoopCount is above the configured maximum.
func (self *PipelineConfig) PipelinePack(msgLoopCount uint) *PipelinePack {
	if msgLoopCount++; msgLoopCount > self.Globals.MaxMsgLoops {
		return nil
	}
	pack := <-self.injectRecycleChan
	pack.Message.SetTimestamp(time.Now().UnixNano())
	pack.Message.SetUuid(uuid.NewRandom())
	pack.Message.SetHostname(self.hostname)
	pack.Message.SetPid(self.pid)
	pack.RefCount = 1
	pack.MsgLoopCount = msgLoopCount
	return pack
}

// Returns the router.
func (self *PipelineConfig) Router() MessageRouter {
	return self.router
}

// Returns the inputRecycleChannel.
func (self *PipelineConfig) InputRecycleChan() chan *PipelinePack {
	return self.inputRecycleChan
}

// Returns the injectRecycleChannel.
func (self *PipelineConfig) InjectRecycleChan() chan *PipelinePack {
	return self.injectRecycleChan
}

// Returns the hostname.
func (self *PipelineConfig) Hostname() string {
	return self.hostname
}

// Returns OutputRunner registered under the specified name, or nil (and ok ==
// false) if no such name is registered.
func (self *PipelineConfig) Output(name string) (oRunner OutputRunner, ok bool) {
	oRunner, ok = self.OutputRunners[name]
	return
}

// Returns the underlying config object via the Helper interface.
func (self *PipelineConfig) PipelineConfig() *PipelineConfig {
	return self
}

// Instantiates and returns a Decoder of the specified name. Note that any
// time this method is used to fetch an unwrapped Decoder instance, it is up
// to the caller to check for and possibly satisfy the WantsDecoderRunner and
// WantsDecoderRunnerShutdown interfaces.
func (self *PipelineConfig) Decoder(name string) (decoder Decoder, ok bool) {
	var maker PluginMaker
	self.makersLock.RLock()
	defer self.makersLock.RUnlock()
	if maker, ok = self.DecoderMakers[name]; !ok {
		return
	}

	plugin, err := maker.Make()
	if err != nil {
		return nil, false
	}
	decoder = plugin.(Decoder)
	return
}

// Instantiates, starts, and returns a DecoderRunner wrapped around a newly
// created Decoder of the specified name.
func (self *PipelineConfig) DecoderRunner(baseName, fullName string) (
	dRunner DecoderRunner, ok bool) {

	self.makersLock.RLock()
	var maker PluginMaker
	if maker, ok = self.DecoderMakers[baseName]; !ok {
		self.makersLock.RUnlock()
		return
	}

	runner, err := maker.MakeRunner(fullName)
	self.makersLock.RUnlock()
	if err != nil {
		return nil, false
	}

	dRunner = runner.(DecoderRunner)
	self.allDecodersLock.Lock()
	self.allDecoders = append(self.allDecoders, dRunner)
	self.allDecodersLock.Unlock()
	self.decodersWg.Add(1)
	dRunner.Start(self, &self.decodersWg)
	return
}

// Stops and unregisters the provided DecoderRunner.
func (self *PipelineConfig) StopDecoderRunner(dRunner DecoderRunner) (ok bool) {
	self.allDecodersLock.Lock()
	defer self.allDecodersLock.Unlock()
	for i, r := range self.allDecoders {
		if r == dRunner {
			close(dRunner.InChan())
			self.allDecoders = append(self.allDecoders[:i], self.allDecoders[i+1:]...)
			ok = true
			break
		}
	}
	return
}

// Instantiates and returns an Encoder of the specified name.
func (self *PipelineConfig) Encoder(baseName, fullName string) (Encoder, bool) {
	self.makersLock.RLock()
	maker, ok := self.makers["Encoder"][baseName]
	if !ok {
		self.makersLock.RUnlock()
		return nil, false
	}

	plugin, err := maker.Make()
	self.makersLock.RUnlock()
	if err != nil {
		msg := fmt.Sprintf("Error creating encoder '%s': %s", fullName, err.Error())
		self.log(msg)
	}
	encoder := plugin.(Encoder)
	if wantsName, ok := encoder.(WantsName); ok {
		wantsName.SetName(fullName)
	}
	self.allEncodersLock.Lock()
	self.allEncoders[fullName] = encoder
	self.allEncodersLock.Unlock()
	return encoder, true
}

// Returns a FilterRunner with the given name, or nil and ok == false if no
// such name is registered.
func (self *PipelineConfig) Filter(name string) (fRunner FilterRunner, ok bool) {
	self.filtersLock.RLock()
	defer self.filtersLock.RUnlock()
	fRunner, ok = self.FilterRunners[name]
	return
}

// Returns the specified StatAccumulator input plugin, or an error if it can't
// be found.
func (self *PipelineConfig) StatAccumulator(name string) (statAccum StatAccumulator,
	err error) {

	self.inputsLock.RLock()
	defer self.inputsLock.RUnlock()
	iRunner, ok := self.InputRunners[name]
	if !ok {
		err = fmt.Errorf("No Input named '%s", name)
		return
	}
	input := iRunner.Input()
	if statAccum, ok = input.(StatAccumulator); !ok {
		err = fmt.Errorf("Input '%s' is not a StatAccumulator", name)
	}
	return
}

// Starts the provided FilterRunner and adds it to the set of running Filters.
func (self *PipelineConfig) AddFilterRunner(fRunner FilterRunner) error {
	self.filtersLock.Lock()
	defer self.filtersLock.Unlock()
	self.FilterRunners[fRunner.Name()] = fRunner
	self.filtersWg.Add(1)
	if err := fRunner.Start(self, &self.filtersWg); err != nil {
		self.filtersWg.Done()
		return fmt.Errorf("AddFilterRunner '%s' failed to start: %s",
			fRunner.Name(), err)
	} else {
		self.router.AddFilterMatcher() <- fRunner.MatchRunner()
	}
	return nil
}

// Removes the specified FilterRunner from the configuration and the
// MessageRouter which signals the filter to shutdown by closing the input
// channel. Returns true if the filter was removed.
func (self *PipelineConfig) RemoveFilterRunner(name string) bool {
	if self.Globals.IsShuttingDown() {
		return false
	}

	self.filtersLock.Lock()
	defer self.filtersLock.Unlock()
	if fRunner, ok := self.FilterRunners[name]; ok {
		self.router.RemoveFilterMatcher() <- fRunner.MatchRunner()
		delete(self.FilterRunners, name)
		return true
	}
	return false
}

// AddInputRunner Starts the provided InputRunner and adds it to the set of
// running Inputs.
func (self *PipelineConfig) AddInputRunner(iRunner InputRunner) error {
	self.inputsLock.Lock()
	defer self.inputsLock.Unlock()
	self.InputRunners[iRunner.Name()] = iRunner
	self.inputsWg.Add(1)
	if err := iRunner.Start(self, &self.inputsWg); err != nil {
		self.inputsWg.Done()
		return fmt.Errorf("AddInputRunner '%s' failed to start: %s", iRunner.Name(), err)
	}
	return nil
}

// RemoveInputRunner unregisters the provided InputRunner, and stops it.
func (self *PipelineConfig) RemoveInputRunner(iRunner InputRunner) {
	name := iRunner.Name()
	self.makersLock.Lock()
	inputMakers := self.makers["Input"]
	if _, ok := inputMakers[name]; ok {
		delete(inputMakers, name)
	}
	self.makersLock.Unlock()

	self.inputsLock.Lock()
	delete(self.InputRunners, name)
	self.inputsLock.Unlock()

	iRunner.Input().Stop()
}

// RemoveOutputRunner unregisters the provided OutputRunner from heka, and
// removes it's message matcher from the heka router.
func (self *PipelineConfig) RemoveOutputRunner(oRunner OutputRunner) {
	name := oRunner.Name()
	self.makersLock.Lock()
	outputMakers := self.makers["Output"]
	if _, ok := outputMakers[name]; ok {
		self.router.RemoveOutputMatcher() <- oRunner.MatchRunner()
		delete(outputMakers, name)
	}
	self.makersLock.Unlock()

	self.outputsLock.Lock()
	delete(self.OutputRunners, name)
	self.outputsLock.Unlock()
}

type ConfigFile PluginConfig

// This struct provides a structure for the available retry options for
// a plugin that supports being restarted
type RetryOptions struct {
	// Maximum time in seconds between restart attempts. Defaults to 30s.
	MaxDelay string `toml:"max_delay"`
	// Starting delay in milliseconds between restart attempts. Defaults to
	// 250ms.
	Delay string
	// Maximum jitter added to every retry attempt. Defaults to 500ms.
	MaxJitter string `toml:"max_jitter"`
	// How many times to attempt starting the plugin before failing. Defaults
	// to -1 (retry forever).
	MaxRetries int `toml:"max_retries"`
}

var unknownOptionRegex = regexp.MustCompile("^Configuration contains key \\[(?P<key>\\S+)\\]")

// Uses reflection to extract an attribute value from an arbitrary struct type
// that may or may not actually have the attribute, returning a provided
// default if the provided object is not a struct or if the attribute doesn't
// exist.
func getAttr(ob interface{}, attr string, default_ interface{}) (ret interface{}) {
	ret = default_
	obVal := reflect.ValueOf(ob)
	obVal = reflect.Indirect(obVal) // Dereference if it's a pointer.
	if obVal.Kind().String() != "struct" {
		// `FieldByName` will panic if we're not a struct.
		return
	}
	attrVal := obVal.FieldByName(attr)
	if !attrVal.IsValid() {
		return
	}
	return attrVal.Interface()
}

// Used internally to log and record plugin config loading errors.
func (self *PipelineConfig) log(msg string) {
	self.LogMsgs = append(self.LogMsgs, msg)
	log.Println(msg)
}

var PluginTypeRegex = regexp.MustCompile("(Decoder|Encoder|Filter|Input|Output)$")

func getPluginCategory(pluginType string) string {
	pluginCats := PluginTypeRegex.FindStringSubmatch(pluginType)
	if len(pluginCats) < 2 {
		return ""
	}
	return pluginCats[1]
}

type CommonConfig struct {
	Typ string `toml:"type"`
}

type CommonInputConfig struct {
	Ticker             uint `toml:"ticker_interval"`
	Decoder            string
	SyncDecode         *bool `toml:"synchronous_decode"`
	SendDecodeFailures *bool `toml:"send_decode_failures"`
	Retries            RetryOptions
}

type CommonFOConfig struct {
	Ticker     uint   `toml:"ticker_interval"`
	Matcher    string `toml:"message_matcher"`
	Signer     string `toml:"message_signer"`
	CanExit    *bool  `toml:"can_exit"`
	Retries    RetryOptions
	Encoder    string // Output only.
	UseFraming *bool  `toml:"use_framing"` // Output only.
}

func getDefaultRetryOptions() RetryOptions {
	return RetryOptions{
		MaxDelay:   "30s",
		Delay:      "250ms",
		MaxRetries: -1,
	}
}

type PluginMaker interface {
	Name() string
	Type() string
	Category() string
	Config() interface{}
	PrepConfig() error
	Make() (Plugin, error)
	MakeRunner(name string) (PluginRunner, error)
}

// MutableMaker is for consumers that want to customize the behavior of the
// PluginMaker in "bad touch" ways, use at your own risk. Both interfaces are
// implemented by the same struct, so a PluginMaker can be turned into a
// MutableMaker via type coersion.
type MutableMaker interface {
	PluginMaker
	SetConfig(config interface{})
	CommonTypedConfig() interface{}
	SetCommonTypedConfig(config interface{})
	SetName(name string)
	SetType(typ string)
	SetCategory(category string)
}

type pluginMaker struct {
	name              string
	category          string
	tomlSection       toml.Primitive
	commonConfig      CommonConfig
	commonTypedConfig interface{}
	pConfig           *PipelineConfig
	constructor       func() interface{}
	configStruct      interface{}
	configPrepped     bool
	plugin            Plugin
}

// NewPluginMaker creates and returns a PluginMaker that can generate running
// plugins for the provided TOML configuration. It will load the plugin type
// and extract any of the Heka-defined common config for the plugin before
// returning.
func NewPluginMaker(name string, pConfig *PipelineConfig, tomlSection toml.Primitive) (
	PluginMaker, error) {

	// Create the maker, extract plugin type, and make sure the plugin type
	// exists.
	maker := &pluginMaker{
		name:         name,
		tomlSection:  tomlSection,
		commonConfig: CommonConfig{},
		pConfig:      pConfig,
	}

	var err error
	if err = toml.PrimitiveDecode(tomlSection, &maker.commonConfig); err != nil {
		return nil, fmt.Errorf("can't decode common config for '%s': %s", name, err)
	}
	if maker.commonConfig.Typ == "" {
		maker.commonConfig.Typ = name
	}
	constructor, ok := AvailablePlugins[maker.commonConfig.Typ]
	if !ok {
		return nil, fmt.Errorf("No registered plugin type: %s", maker.commonConfig.Typ)
	}
	maker.constructor = constructor

	// Extract plugin category and any category-specific common (i.e. Heka
	// defined) configuration.
	maker.category = getPluginCategory(maker.commonConfig.Typ)
	if maker.category == "" {
		return nil, errors.New("Unrecognized plugin category")
	}

	switch maker.category {
	case "Input":
		commonInput := CommonInputConfig{
			Retries: getDefaultRetryOptions(),
		}
		err = toml.PrimitiveDecode(tomlSection, &commonInput)
		maker.commonTypedConfig = commonInput
	case "Filter", "Output":
		commonFO := CommonFOConfig{
			Retries: getDefaultRetryOptions(),
		}
		err = toml.PrimitiveDecode(tomlSection, &commonFO)
		maker.commonTypedConfig = commonFO
	}

	if err != nil {
		return nil, fmt.Errorf("can't decode common %s config for '%s': %s",
			strings.ToLower(maker.category), name, err)
	}

	return maker, nil
}

func (m *pluginMaker) Name() string {
	return m.name
}

func (m *pluginMaker) Type() string {
	return m.commonConfig.Typ
}

func (m *pluginMaker) Category() string {
	return m.category
}

func (m *pluginMaker) SetName(name string) {
	m.name = name
}

func (m *pluginMaker) SetType(typ string) {
	m.commonConfig.Typ = typ
}

func (m *pluginMaker) SetCategory(category string) {
	m.category = category
}

// makePlugin instantiates a plugin instance, provides name and pConfig to the
// plugin if necessary, and returns the plugin.
func (m *pluginMaker) makePlugin() Plugin {
	plugin := m.constructor().(Plugin)
	if wantsPConfig, ok := plugin.(WantsPipelineConfig); ok {
		wantsPConfig.SetPipelineConfig(m.pConfig)
	}
	if wantsName, ok := plugin.(WantsName); ok {
		wantsName.SetName(m.name)
	}
	return plugin
}

// makeConfig calls makePlugin to create a plugin instance, uses that instance
// to create a config object, and then stores the plugin and the created
// config object as attributes on the pluginMaker struct.

func (m *pluginMaker) makeConfig() {
	if m.plugin == nil {
		m.plugin = m.makePlugin()
	}
	hasConfigStruct, ok := m.plugin.(HasConfigStruct)
	if ok {
		m.configStruct = hasConfigStruct.ConfigStruct()
	} else {
		// If we don't have a config struct, fall back to a PluginConfig.
		m.configStruct = PluginConfig{}
	}
}

// Config returns the PluginMaker's config struct, creating it if necessary.
func (m *pluginMaker) Config() interface{} {
	if m.configStruct == nil {
		m.makeConfig()
	}
	return m.configStruct
}

func (m *pluginMaker) CommonTypedConfig() interface{} {
	return m.commonTypedConfig
}

func (m *pluginMaker) SetConfig(config interface{}) {
	m.configStruct = config
}

func (m *pluginMaker) SetCommonTypedConfig(config interface{}) {
	m.commonTypedConfig = config
}

// PrepConfig generates a config struct for the plugin (instantiating an
// instance of the plugin to do so, if necessary) and decodes the TOML config
// into the generated struct.
func (m *pluginMaker) PrepConfig() error {
	if m.configPrepped {
		// Already done, just return.
		return nil
	}

	if m.configStruct == nil {
		m.makeConfig()
	} else if m.plugin == nil {
		m.plugin = m.makePlugin()
	}

	if _, ok := m.plugin.(HasConfigStruct); !ok {
		// If plugin doesn't implement HasConfigStruct then we're decoding
		// into an empty PluginConfig object.
		if err := toml.PrimitiveDecode(m.tomlSection, m.configStruct); err != nil {
			return fmt.Errorf("can't decode config for '%s': %s ", m.name, err.Error())
		}
		m.configPrepped = true
		return nil
	}

	// Use reflection to extract the fields (or TOML tag names, if available)
	// of the values that Heka has already extracted so we know they're not
	// required to be specified in the config struct.
	hekaParams := make(map[string]interface{})
	commons := []interface{}{m.commonConfig, m.commonTypedConfig}
	for _, common := range commons {
		if common == nil {
			continue
		}
		rt := reflect.ValueOf(common).Type()
		for i := 0; i < rt.NumField(); i++ {
			sft := rt.Field(i)
			kname := sft.Tag.Get("toml")
			if len(kname) == 0 {
				kname = sft.Name
			}
			hekaParams[kname] = true
		}
	}

	// Finally decode the TOML into the struct. Use of PrimitiveDecodeStrict
	// means that an error will be raised for any config options in the TOML
	// that don't have corresponding attributes on the struct, delta the
	// hekaParams that can be safely excluded.
	err := toml.PrimitiveDecodeStrict(m.tomlSection, m.configStruct, hekaParams)
	if err != nil {
		matches := unknownOptionRegex.FindStringSubmatch(err.Error())
		if len(matches) == 2 {
			// We've got an unrecognized config option.
			return fmt.Errorf("unknown config setting for '%s': %s", m.name, matches[1])
		}
		return err
	}

	m.configPrepped = true
	return nil
}

// Make initializes a plugin instance using the prepped config struct and
// returns the initialized plugin. If the config hasn't been prepped the
// PrepConfig will be called. A new plugin may be instantiated, or a cached
// plugin may be used, but Make will always return a "fresh" instance, i.e. it
// will never return the same plugin instance twice.
func (m *pluginMaker) Make() (Plugin, error) {
	if !m.configPrepped {
		// Our config struct hasn't been prepped.
		if err := m.PrepConfig(); err != nil {
			return nil, err
		}
	}

	var plugin Plugin
	if m.plugin == nil {
		plugin = m.makePlugin()
	} else {
		plugin = m.plugin
		m.plugin = nil
	}

	if err := plugin.Init(m.configStruct); err != nil {
		return nil, fmt.Errorf("Initialization failed for '%s': %s", m.name, err)
	}

	return plugin, nil
}

// MakeRunner returns a new, unstarted PluginRunner wrapped around a new,
// configured plugin instance. If name is provided, then the Runner will be
// given the specified name; if name is an empty string, the plugin name will
// be used.
func (m *pluginMaker) MakeRunner(name string) (PluginRunner, error) {
	if m.category == "Encoder" {
		return nil, errors.New("Encoder plugins don't support PluginRunners")
	}

	plugin, err := m.Make()
	if err != nil {
		return nil, err
	}

	if name == "" {
		name = m.name
	}

	var runner PluginRunner

	if m.category == "Decoder" {
		runner = NewDecoderRunner(name, plugin.(Decoder), m.pConfig.Globals.PluginChanSize)
		return runner, nil
	}

	// In some cases a plugin implementation will specify a default value for
	// one or more common config settings by including values for those
	// settings in the config struct. We extract them in this function's outer
	// scope, but we only need to use them if the common config hasn't already
	// been populated by values from the TOML.
	defaultTickerInterval := getAttr(m.configStruct, "TickerInterval", uint(0))

	if m.category == "Input" {
		commonInput := m.commonTypedConfig.(CommonInputConfig)
		if commonInput.Ticker == 0 {
			commonInput.Ticker = defaultTickerInterval.(uint)
		}

		// Boolean types are tricky, we use pointer types to distinguish btn
		// false and not set, but a plugin's config struct might not be so
		// smart, so we have to account for both cases.
		if commonInput.SyncDecode == nil {
			syncDecode := getAttr(m.configStruct, "SyncDecode", false)
			switch syncDecode := syncDecode.(type) {
			case bool:
				commonInput.SyncDecode = &syncDecode
			case *bool:
				if syncDecode == nil {
					b := false
					syncDecode = &b
				}
				commonInput.SyncDecode = syncDecode
			}
		}

		if commonInput.SendDecodeFailures == nil {
			sendFailures := getAttr(m.configStruct, "SendDecodeFailures", false)
			switch sendFailures := sendFailures.(type) {
			case bool:
				commonInput.SendDecodeFailures = &sendFailures
			case *bool:
				if sendFailures == nil {
					b := false
					sendFailures = &b
				}
				commonInput.SendDecodeFailures = sendFailures
			}
		}

		if commonInput.Decoder == "" {
			decoder := getAttr(m.configStruct, "Decoder", "")
			commonInput.Decoder = decoder.(string)
		}

		runner = NewInputRunner(name, plugin.(Input), commonInput)
		return runner, nil
	}

	// We're a filter or an output.
	commonFO := m.commonTypedConfig.(CommonFOConfig)

	// More checks for plugin-specified default values of common config
	// settings.
	if commonFO.Matcher == "" {
		matcherVal := getAttr(m.configStruct, "MessageMatcher", "")
		commonFO.Matcher = matcherVal.(string)
	}

	if commonFO.Ticker == 0 {
		commonFO.Ticker = defaultTickerInterval.(uint)
	}

	// Boolean types are tricky, we use pointer types to distinguish btn false
	// and not set, but a plugin's config struct might not be so smart, so we
	// have to account for both cases.
	if commonFO.CanExit == nil {
		canExit := getAttr(m.configStruct, "CanExit", false)
		switch canExit := canExit.(type) {
		case bool:
			commonFO.CanExit = &canExit
		case *bool:
			if canExit == nil {
				b := false
				canExit = &b
			}
			commonFO.CanExit = canExit
		}
	}

	if m.category == "Output" {
		if commonFO.UseFraming == nil {
			useFraming := getAttr(m.configStruct, "UseFraming", false)
			switch useFraming := useFraming.(type) {
			case bool:
				commonFO.UseFraming = &useFraming
			case *bool:
				if useFraming == nil {
					b := false
					useFraming = &b
				}
				commonFO.UseFraming = useFraming
			}
		}

		if commonFO.Encoder == "" {
			encoder := getAttr(m.configStruct, "Encoder", "")
			commonFO.Encoder = encoder.(string)
		}
	}

	return NewFORunner(name, plugin, commonFO, m.commonConfig.Typ,
		m.pConfig.Globals.PluginChanSize)
}

// Default protobuf configurations.
const protobufDecoderToml = `
[ProtobufDecoder]
`

const protobufEncoderToml = `
[ProtobufEncoder]
`

// Loads all plugin configuration from a TOML configuration file. The
// PipelineConfig should be already initialized via the Init function before
// this method is called.
func (self *PipelineConfig) LoadFromConfigFile(filename string) error {
	var (
		configFile ConfigFile
		err        error
	)

	contents, err := ReplaceEnvsFile(filename)
	if err != nil {
		return err
	}

	if _, err = toml.Decode(contents, &configFile); err != nil {
		return fmt.Errorf("Error decoding config file: %s", err)
	}

	var (
		errcnt              uint
		protobufDRegistered bool
		protobufERegistered bool
	)
	makersByCategory := make(map[string][]PluginMaker)

	// Load all the plugin makers and file them by category.
	for name, conf := range configFile {
		if name == HEKA_DAEMON {
			continue
		}
		log.Printf("Pre-loading: [%s]\n", name)
		maker, err := NewPluginMaker(name, self, conf)
		if err != nil {
			self.log(err.Error())
			errcnt++
			continue
		}

		if maker.Type() == "MultiDecoder" {
			// Special case MultiDecoders so we can make sure they get
			// registered *after* all possible subdecoders.
			makersByCategory["MultiDecoder"] = append(makersByCategory["MultiDecoder"],
				maker)
		} else {
			category := maker.Category()
			makersByCategory[category] = append(makersByCategory[category], maker)
		}
		if maker.Name() == "ProtobufDecoder" {
			protobufDRegistered = true
		}
		if maker.Name() == "ProtobufEncoder" {
			protobufERegistered = true
		}
	}

	// Make sure ProtobufDecoder is registered.
	if !protobufDRegistered {
		var configDefault ConfigFile
		toml.Decode(protobufDecoderToml, &configDefault)
		log.Println("Pre-loading: [ProtobufDecoder]")
		maker, err := NewPluginMaker("ProtobufDecoder", self,
			configDefault["ProtobufDecoder"])
		if err != nil {
			// This really shouldn't happen.
			self.log(err.Error())
			errcnt++
		} else {
			makersByCategory["Decoder"] = append(makersByCategory["Decoder"],
				maker)
		}
	}

	// Make sure ProtobufEncoder is registered.
	if !protobufERegistered {
		var configDefault ConfigFile
		toml.Decode(protobufEncoderToml, &configDefault)
		log.Println("Pre-loading: [ProtobufEncoder]")
		maker, err := NewPluginMaker("ProtobufEncoder", self,
			configDefault["ProtobufEncoder"])
		if err != nil {
			// This really shouldn't happen.
			self.log(err.Error())
			errcnt++
		} else {
			makersByCategory["Encoder"] = append(makersByCategory["Encoder"],
				maker)
		}
	}

	multiDecoders := make([]multiDecoderNode, len(makersByCategory["MultiDecoder"]))
	multiMakers := make(map[string]PluginMaker)
	for i, maker := range makersByCategory["MultiDecoder"] {
		multiMakers[maker.Name()] = maker
		tomlSection := maker.(*pluginMaker).tomlSection
		multiDecoders[i] = newMultiDecoderNode(maker.Name(), subsFromSection(tomlSection))
	}
	multiDecoders, err = orderDependencies(multiDecoders)
	if err != nil {
		return err
	}
	for i, d := range multiDecoders {
		makersByCategory["MultiDecoder"][i] = multiMakers[d.name]
	}

	// Append MultiDecoders to the end of the Decoders list.
	makersByCategory["Decoder"] = append(makersByCategory["Decoder"],
		makersByCategory["MultiDecoder"]...)

	// Force decoders and encoders to be loaded before the other plugin
	// types are initialized so we know they'll be there for inputs and
	// outputs to use during initialization.
	order := []string{"Decoder", "Encoder", "Input", "Filter", "Output"}
	for _, category := range order {
		for _, maker := range makersByCategory[category] {
			log.Printf("Loading: [%s]\n", maker.Name())
			if err = maker.PrepConfig(); err != nil {
				self.log(err.Error())
				errcnt++
			}
			self.makers[category][maker.Name()] = maker
			if category == "Encoder" {
				continue
			}
			runner, err := maker.MakeRunner("")
			if err != nil {
				// Might be a duplicate error.
				seen := false
				for _, prevErr := range self.LogMsgs {
					if err.Error() == prevErr {
						seen = true
						break
					}
				}
				if !seen {
					msg := fmt.Sprintf("Error making runner for %s: %s", maker.Name(),
						err.Error())
					self.log(msg)
					errcnt++
				}
				continue
			}
			switch category {
			case "Input":
				self.InputRunners[maker.Name()] = runner.(InputRunner)
			case "Filter":
				self.FilterRunners[maker.Name()] = runner.(FilterRunner)
			case "Output":
				self.OutputRunners[maker.Name()] = runner.(OutputRunner)
			}
		}
	}

	if errcnt != 0 {
		return fmt.Errorf("%d errors loading plugins", errcnt)
	}

	return nil
}

func subsFromSection(section toml.Primitive) []string {
	secMap := section.(map[string]interface{})
	var subs []string
	if _, ok := secMap["subs"]; ok {
		subsUntyped, _ := secMap["subs"].([]interface{})
		subs = make([]string, len(subsUntyped))
		for i, subUntyped := range subsUntyped {
			subs[i], _ = subUntyped.(string)
		}
	}
	return subs
}

func ReplaceEnvsFile(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	r, err := EnvSub(file)
	if err != nil {
		return "", err
	}
	contents, err := ioutil.ReadAll(r)
	if err != nil {
		return "", err
	}
	return string(contents), nil
}

func EnvSub(r io.Reader) (io.Reader, error) {
	bufIn := bufio.NewReader(r)
	bufOut := new(bytes.Buffer)
	for {
		chunk, err := bufIn.ReadBytes(byte('%'))
		if err != nil {
			if err == io.EOF {
				// We're done.
				bufOut.Write(chunk)
				break
			}
			return nil, err
		}
		bufOut.Write(chunk[:len(chunk)-1])

		tmp := make([]byte, 4)
		tmp, err = bufIn.Peek(4)
		if err != nil {
			if err == io.EOF {
				// End of file, write the last few bytes out and exit.
				bufOut.WriteRune('%')
				bufOut.Write(tmp)
				break
			}
			return nil, err
		}

		if string(tmp) == "ENV[" {
			// Found opening delimiter, advance the read cursor and look for
			// closing delimiter.
			tmp, err = bufIn.ReadBytes(byte('['))
			if err != nil {
				// This shouldn't happen, since the Peek succeeded.
				return nil, err
			}
			chunk, err = bufIn.ReadBytes(byte(']'))
			if err != nil {
				if err == io.EOF {
					// No closing delimiter, return an error
					return nil, ErrMissingCloseDelim
				}
				return nil, err
			}
			// `chunk` is now holding var name + closing delimiter.
			// var name contains invalid characters, return an error
			if bytes.IndexAny(chunk, invalidEnvChars) != -1 ||
				bytes.Index(chunk, invalidEnvPrefix) != -1 {
				return nil, ErrInvalidChars
			}
			varName := string(chunk[:len(chunk)-1])
			varVal := os.Getenv(varName)
			bufOut.WriteString(varVal)
		} else {
			// Just a random '%', not an opening delimiter, write it out and
			// keep going.
			bufOut.WriteRune('%')
		}
	}
	return bufOut, nil
}
