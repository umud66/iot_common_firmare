package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	mqttclient "github.com/eclipse/paho.mqtt.golang"
)

type simulatorConfig struct {
	APIBaseURL        string `json:"apiBaseUrl"`
	DeviceID          string `json:"deviceId"`
	Platform          string `json:"platform"`
	MQTTBroker        string `json:"mqttBroker,omitempty"`
	ClientID          string `json:"clientId"`
	ReportIntervalSec int    `json:"reportIntervalSec"`
	DeviceToken       string `json:"deviceToken,omitempty"`
	ConfigVersion     int    `json:"configVersion"`
}

type runtimeChannel struct {
	TargetID     string
	Kind         string
	GPIO         int
	State        string
	Duty         int
	Mode         string
	TemperatureC float64
	Status       string
}

type pwmRuntime struct {
	Mode        string
	Params      map[string]any
	StartedAt   time.Time
	LastRunAt   time.Time
	Completed   bool
	RandomSeed  uint64
	RandomDuty  int
	CurrentDuty float64
}

func loopCountValue(params map[string]any) int {
	value, ok := params["loop"]
	if !ok {
		return 0
	}
	switch item := value.(type) {
	case bool:
		if item {
			return -1
		}
		return 0
	case int:
		return item
	case int32:
		return int(item)
	case int64:
		return int(item)
	case float32:
		return int(item)
	case float64:
		return int(item)
	default:
		return 0
	}
}

func directionValue(params map[string]any, fallback string) string {
	value, ok := params["direction"].(string)
	if ok && value != "" {
		return value
	}
	return fallback
}

func normFloat(value float64, start float64, end float64) float64 {
	if end == start {
		return 0
	}
	return clampFloat((value-start)/(end-start), 0, 1)
}

func curveProgress(progress float64, curve string, fromDuty int, control1 int, control2 int, toDuty int) float64 {
	progress = clampFloat(progress, 0, 1)
	switch curve {
	case "", "linear":
		return progress
	case "easeIn":
		return progress * progress * progress
	case "easeOut":
		inv := 1 - progress
		return 1 - inv*inv*inv
	case "easeInOut":
		if progress < 0.5 {
			return 4 * progress * progress * progress
		}
		inv := -2*progress + 2
		return 1 - (inv*inv*inv)/2
	case "smooth":
		return progress * progress * (3 - 2*progress)
	case "sineIn":
		return 1 - math.Cos((progress*math.Pi)/2)
	case "sineOut":
		return math.Sin((progress * math.Pi) / 2)
	case "sineInOut":
		return -(math.Cos(math.Pi*progress) - 1) / 2
	case "backIn":
		const c1 = 1.70158
		const c3 = c1 + 1
		return c3*progress*progress*progress - c1*progress*progress
	case "backOut":
		const c1 = 1.70158
		const c3 = c1 + 1
		p := progress - 1
		return 1 + c3*p*p*p + c1*p*p
	case "customBezier":
		value := cubicBezier(progress, float64(fromDuty), float64(control1), float64(control2), float64(toDuty))
		return normFloat(value, float64(fromDuty), float64(toDuty))
	default:
		return progress
	}
}

type transportConfig struct {
	Protocol    string `json:"protocol"`
	TopicPrefix string `json:"topicPrefix"`
	BrokerURL   string `json:"brokerUrl,omitempty"`
}

type capability struct {
	ID           string `json:"id"`
	Kind         string `json:"kind"`
	GPIO         int    `json:"gpio,omitempty"`
	DefaultState string `json:"defaultState,omitempty"`
}

type sequenceStep struct {
	Duty       int `json:"duty"`
	DurationMs int `json:"durationMs"`
}

type sequenceChannel struct {
	TargetID string         `json:"targetId"`
	Steps    []sequenceStep `json:"steps"`
}

type timerAction struct {
	Function      string            `json:"function,omitempty"`
	Mode          string            `json:"mode"`
	State         string            `json:"state,omitempty"`
	Duty          int               `json:"duty,omitempty"`
	From          int               `json:"from,omitempty"`
	To            int               `json:"to,omitempty"`
	MinDuty       int               `json:"minDuty,omitempty"`
	MaxDuty       int               `json:"maxDuty,omitempty"`
	LowDuty       int               `json:"lowDuty,omitempty"`
	HighDuty      int               `json:"highDuty,omitempty"`
	Control1      int               `json:"control1,omitempty"`
	Control2      int               `json:"control2,omitempty"`
	DurationMs    int               `json:"durationMs,omitempty"`
	IntervalMs    int               `json:"intervalMs,omitempty"`
	PeriodMs      int               `json:"periodMs,omitempty"`
	OnDurationMs  int               `json:"onDurationMs,omitempty"`
	OffDurationMs int               `json:"offDurationMs,omitempty"`
	Repeat        int               `json:"repeat,omitempty"`
	Smoothing     int               `json:"smoothing,omitempty"`
	Curve         string            `json:"curve,omitempty"`
	Direction     string            `json:"direction,omitempty"`
	Loop          int               `json:"loop,omitempty"`
	Steps         []sequenceStep    `json:"steps,omitempty"`
	Channels      []sequenceChannel `json:"channels,omitempty"`
}

type weeklyTimer struct {
	ID         string      `json:"id"`
	Enabled    bool        `json:"enabled"`
	At         string      `json:"at"`
	DaysOfWeek []int       `json:"daysOfWeek"`
	Action     timerAction `json:"action"`
}

type portTimerGroup struct {
	TargetID string        `json:"targetId"`
	Timers   []weeklyTimer `json:"timers"`
}

type gpioBinding struct {
	ID      string `json:"id"`
	GPIO    int    `json:"gpio"`
	PinRole string `json:"pinRole"`
}

type driverInstance struct {
	ID                 string         `json:"id"`
	DriverDefinitionID string         `json:"driverDefinitionId"`
	TargetID           string         `json:"targetId"`
	DisplayName        string         `json:"displayName"`
	Config             map[string]any `json:"config,omitempty"`
	GPIOBindings       []gpioBinding  `json:"gpioBindings,omitempty"`
}

type deviceInfo struct {
	DeviceID     string           `json:"deviceId"`
	Name         string           `json:"name"`
	DeviceType   string           `json:"deviceType"`
	Transport    transportConfig  `json:"transport"`
	Capabilities []capability     `json:"capabilities"`
	TimerGroups  []portTimerGroup `json:"timerGroups,omitempty"`
	Metadata     map[string]any   `json:"metadata,omitempty"`
}

type bootstrapPayload struct {
	Device          deviceInfo       `json:"device"`
	DriverInstances []driverInstance `json:"driverInstances,omitempty"`
	DownlinkTopic   string           `json:"downlinkTopic"`
	UpstreamTopic   string           `json:"upstreamTopic"`
}

type driverCommand struct {
	TargetID  string         `json:"targetId"`
	Kind      string         `json:"kind"`
	Operation string         `json:"operation"`
	Params    map[string]any `json:"params,omitempty"`
}

type plannedPayload struct {
	DeviceID      string        `json:"deviceId"`
	TemplateID    string        `json:"templateId,omitempty"`
	SentAt        string        `json:"sentAt"`
	CommandSchema string        `json:"commandSchema,omitempty"`
	Command       driverCommand `json:"command"`
}

type channelStateReport struct {
	TargetID     string  `json:"targetId"`
	Kind         string  `json:"kind"`
	State        string  `json:"state,omitempty"`
	Duty         int     `json:"duty,omitempty"`
	Mode         string  `json:"mode,omitempty"`
	TemperatureC float64 `json:"temperatureC,omitempty"`
	Status       string  `json:"status,omitempty"`
	UpdatedAt    string  `json:"updatedAt,omitempty"`
}

type deviceStateReport struct {
	DeviceID   string                        `json:"deviceId"`
	Online     bool                          `json:"online"`
	Ip         string                        `json:"ip,omitempty"`
	Rssi       int                           `json:"rssi,omitempty"`
	UptimeMs   uint64                        `json:"uptimeMs,omitempty"`
	Channels   map[string]channelStateReport `json:"channels,omitempty"`
	Metrics    map[string]float64            `json:"metrics,omitempty"`
	LastError  string                        `json:"lastError,omitempty"`
	ReportedAt string                        `json:"reportedAt,omitempty"`
}

type pairResponse struct {
	DeviceToken string           `json:"deviceToken"`
	Bootstrap   bootstrapPayload `json:"bootstrap"`
}

type simulatorState struct {
	StateVersion int               `json:"stateVersion"`
	Bootstrap    bootstrapPayload  `json:"bootstrap"`
	LastReport   deviceStateReport `json:"lastReport,omitempty"`
	SavedAt      string            `json:"savedAt"`
}

type simulator struct {
	config     simulatorConfig
	configPath string
	statePath  string
	httpClient *http.Client
	mqttClient mqttclient.Client

	mu           sync.Mutex
	deviceToken  string
	bootstrap    bootstrapPayload
	channels     map[string]*runtimeChannel
	startedAt    time.Time
	reportTicker *time.Ticker
	timerTicker  *time.Ticker
	lastTimerRun map[string]string
	formulas     map[string]*formulaRuntime
	scripts      map[string]*scriptRuntime
	pwmRuntimes  map[string]*pwmRuntime
	metrics      map[string]float64
	lastReport   deviceStateReport
	gpioBackend  gpioBackend
}

func main() {
	configPath := "device-sim.json"
	config, err := loadSimulatorConfig(configPath)
	if err != nil {
		log.Fatalf("读取仿真配置失败: %v", err)
	}

	deviceSimulator := &simulator{
		config:     config,
		configPath: configPath,
		statePath:  strings.TrimSuffix(configPath, filepath.Ext(configPath)) + ".state.json",
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
		channels:     map[string]*runtimeChannel{},
		startedAt:    time.Now(),
		lastTimerRun: map[string]string{},
		formulas:     map[string]*formulaRuntime{},
		scripts:      map[string]*scriptRuntime{},
		pwmRuntimes:  map[string]*pwmRuntime{},
		metrics:      map[string]float64{},
		gpioBackend:  consoleGPIOBackend{},
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	if err := deviceSimulator.bootstrapSession(ctx); err != nil {
		log.Fatalf("启动仿真端失败: %v", err)
	}
	defer deviceSimulator.close()

	<-ctx.Done()
	log.Println("收到退出信号，设备仿真端正在关闭")
}

func (s *simulator) bootstrapSession(ctx context.Context) error {
	if err := s.prepareConfig(); err != nil {
		return err
	}
	if err := s.loadSavedState(); err != nil {
		log.Printf("加载本地仿真状态失败: %v", err)
	}

	if s.config.DeviceToken == "" {
		if err := s.pairDevice(); err != nil {
			if s.hasSavedBootstrap() {
				log.Printf("配对失败，回退到本地已保存配置继续启动: %v", err)
			} else {
				return err
			}
		}
	} else if err := s.fetchBootstrap(); err != nil {
		if s.hasSavedBootstrap() {
			log.Printf("同步服务端功能模块失败，回退到本地已保存配置继续启动: %v", err)
		} else {
			return err
		}
	}
	if !s.hasSavedBootstrap() {
		return fmt.Errorf("当前没有可用的功能模块配置，请先完成配对并同步一次服务端配置")
	}

	s.initializeChannels()
	s.restoreLastChannelState()
	if err := s.connectMQTT(ctx); err != nil {
		return err
	}
	s.startTimerLoop(ctx)
	s.startPeriodicReport(ctx)
	s.logBootstrapSummary()
	return s.publishStateReport("startup")
}

func (s *simulator) pairDevice() error {
	linkCode, err := promptLine("请输入控制器链接码", "")
	if err != nil {
		return err
	}
	body, _ := json.Marshal(map[string]any{
		"deviceId": s.config.DeviceID,
		"linkCode": linkCode,
		"platform": s.config.Platform,
	})
	response, err := s.httpClient.Post(strings.TrimRight(s.config.APIBaseURL, "/")+"/api/public/device/pair", "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return decodeHTTPError(response)
	}
	var payload pairResponse
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		return err
	}
	s.config.DeviceToken = payload.DeviceToken
	s.bootstrap = payload.Bootstrap
	log.Printf("配对成功，deviceId=%s deviceToken=%s", s.config.DeviceID, s.config.DeviceToken)
	if err := s.saveConfig(); err != nil {
		return err
	}
	return s.saveState()
}

func (s *simulator) fetchBootstrap() error {
	bootstrapURL := fmt.Sprintf("%s/api/public/device/bootstrap?deviceId=%s&deviceToken=%s",
		strings.TrimRight(s.config.APIBaseURL, "/"),
		url.QueryEscape(s.config.DeviceID),
		url.QueryEscape(s.config.DeviceToken),
	)
	response, err := s.httpClient.Get(bootstrapURL)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return decodeHTTPError(response)
	}
	if err := json.NewDecoder(response.Body).Decode(&s.bootstrap); err != nil {
		return err
	}
	log.Printf("Bootstrap 成功，downlink=%s upstream=%s", s.bootstrap.DownlinkTopic, s.bootstrap.UpstreamTopic)
	return s.saveState()
}

func (s *simulator) initializeChannels() {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.channels = map[string]*runtimeChannel{}
	s.lastTimerRun = map[string]string{}
	s.formulas = map[string]*formulaRuntime{}
	s.scripts = map[string]*scriptRuntime{}
	s.pwmRuntimes = map[string]*pwmRuntime{}
	s.metrics = map[string]float64{}
	capabilities := s.bootstrap.Device.Capabilities
	if len(s.bootstrap.DriverInstances) > 0 {
		capabilities = capabilitiesFromDriverInstances(s.bootstrap.DriverInstances)
	}
	for _, capability := range capabilities {
		channel := &runtimeChannel{
			TargetID: capability.ID,
			Kind:     capability.Kind,
			GPIO:     capability.GPIO,
			Status:   "ok",
		}
		switch capability.Kind {
		case "relay":
			channel.State = firstNonEmpty(capability.DefaultState, "off")
			channel.Mode = "switch"
		case "mos_pwm":
			channel.Duty = 0
			channel.Mode = "stop"
		case "sensor_temperature":
			channel.TemperatureC = 24.5
			channel.Mode = "read"
		}
		s.channels[capability.ID] = channel
	}
	for _, item := range s.bootstrap.DriverInstances {
		scriptProgram, err := loadScriptProgram(item.Config)
		if err != nil {
			log.Printf("[脚本] 跳过 target=%s，配置解析失败: %v", item.TargetID, err)
			continue
		}
		if scriptProgram != nil {
			runtime, err := newScriptRuntime(*scriptProgram, s)
			if err != nil {
				log.Printf("[脚本] 跳过 target=%s，初始化失败: %v", item.TargetID, err)
				continue
			}
			s.scripts[item.TargetID] = runtime
			log.Printf("[脚本] 已加载 target=%s init=%d loop=%d", item.TargetID, len(scriptProgram.init), len(scriptProgram.loop))
			continue
		}
		program, err := loadFormulaProgram(item.Config)
		if err != nil {
			log.Printf("[公式] 跳过 target=%s，配置解析失败: %v", item.TargetID, err)
			continue
		}
		if program == nil {
			continue
		}
		s.formulas[item.TargetID] = newFormulaRuntime(item.TargetID, *program)
		log.Printf("[公式] 已加载 target=%s rules=%d tickMs=%d", item.TargetID, len(program.Rules), program.TickMs)
	}
}

func (s *simulator) restoreLastChannelState() {
	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.lastReport.Channels) == 0 {
		return
	}
	for targetID, saved := range s.lastReport.Channels {
		channel := s.channels[targetID]
		if channel == nil {
			continue
		}
		switch channel.Kind {
		case "relay":
			channel.State = firstNonEmpty(saved.State, channel.State)
			channel.Mode = firstNonEmpty(saved.Mode, channel.Mode)
			channel.Status = firstNonEmpty(saved.Status, channel.Status)
			_ = s.currentGPIOBackend().WriteRelay(channel.TargetID, channel.GPIO, channel.State)
		case "mos_pwm":
			channel.Duty = saved.Duty
			channel.Mode = firstNonEmpty(saved.Mode, channel.Mode)
			channel.Status = firstNonEmpty(saved.Status, channel.Status)
			_ = s.currentGPIOBackend().WritePWM(channel.TargetID, channel.GPIO, channel.Duty)
		case "sensor_temperature":
			channel.TemperatureC = saved.TemperatureC
			channel.Mode = firstNonEmpty(saved.Mode, channel.Mode)
			channel.Status = firstNonEmpty(saved.Status, channel.Status)
		}
	}
	if len(s.lastReport.Metrics) > 0 {
		s.metrics = cloneMetrics(s.lastReport.Metrics)
	}
	log.Printf("已恢复本地运行状态: channels=%d", len(s.lastReport.Channels))
}

func capabilitiesFromDriverInstances(items []driverInstance) []capability {
	result := make([]capability, 0, len(items))
	for _, item := range items {
		switch item.DriverDefinitionID {
		case "driver-relay-builtin":
			result = append(result, capability{
				ID:           item.TargetID,
				Kind:         "relay",
				GPIO:         gpioFromBindings(item.GPIOBindings, "control"),
				DefaultState: stringValue(item.Config, "defaultPowerOnState", "off"),
			})
		case "driver-mos-pwm-builtin":
			result = append(result, capability{
				ID:   item.TargetID,
				Kind: "mos_pwm",
				GPIO: gpioFromBindings(item.GPIOBindings, "pwm"),
			})
		case "driver-ds18b20-builtin":
			result = append(result, capability{
				ID:   item.TargetID,
				Kind: "sensor_temperature",
				GPIO: gpioFromBindings(item.GPIOBindings, "data"),
			})
		}
	}
	return result
}

func gpioFromBindings(bindings []gpioBinding, role string) int {
	for _, item := range bindings {
		if role == "" || item.PinRole == role {
			return item.GPIO
		}
	}
	return -1
}

func (s *simulator) startTimerLoop(ctx context.Context) {
	s.timerTicker = time.NewTicker(50 * time.Millisecond)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-s.timerTicker.C:
				s.processTimers(now.UTC())
			}
		}
	}()
}

func (s *simulator) processTimers(now time.Time) {
	for _, group := range s.bootstrap.Device.TimerGroups {
		for _, timer := range group.Timers {
			if !timer.Enabled || timer.At == "" {
				continue
			}
			if !allowWeekday(now, timer.DaysOfWeek) {
				continue
			}
			if now.Format("15:04") != timer.At {
				continue
			}
			timerKey := group.TargetID + ":" + timer.ID
			runMark := now.Format("2006-01-02 15:04")
			s.mu.Lock()
			if s.lastTimerRun[timerKey] == runMark {
				s.mu.Unlock()
				continue
			}
			s.lastTimerRun[timerKey] = runMark
			s.mu.Unlock()

			command := s.commandFromTimer(group.TargetID, timer.Action)
			log.Printf("[定时器] target=%s timer=%s at=%s tz=UTC", group.TargetID, timer.ID, now.Format(time.RFC3339))
			if err := s.applyCommand(command); err != nil {
				log.Printf("[定时器] 执行失败: %v", err)
				continue
			}
			if err := s.publishStateReport("timer"); err != nil {
				log.Printf("[定时器] 上报失败: %v", err)
			}
		}
	}
	s.processScripts(now)
	s.processFormulas(now)
	s.processPWMRuntimes(now)
}

func (s *simulator) processScripts(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for targetID, runtime := range s.scripts {
		channel := s.channels[targetID]
		if channel == nil || channel.Mode != "script" {
			continue
		}
		if !runtime.shouldRun(now) {
			continue
		}
		if err := runtime.executeTick(s, now); err != nil {
			log.Printf("[脚本] target=%s 执行失败: %v", targetID, err)
			channel.Status = "script_error"
			continue
		}
		channel.Status = "ok"
	}
}

func (s *simulator) processFormulas(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for targetID, runtime := range s.formulas {
		if !runtime.shouldRun(now) {
			continue
		}
		if err := runtime.execute(s, now); err != nil {
			log.Printf("[公式] target=%s 执行失败: %v", targetID, err)
			if channel := s.channels[targetID]; channel != nil {
				channel.Status = "formula_error"
			}
			continue
		}
		if channel := s.channels[targetID]; channel != nil && channel.Mode == "" {
			channel.Mode = "formula"
		}
	}
}

func (s *simulator) processPWMRuntimes(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for targetID, runtime := range s.pwmRuntimes {
		if runtime == nil || runtime.Completed {
			continue
		}
		channel := s.channels[targetID]
		if channel == nil || channel.Kind != "mos_pwm" {
			continue
		}
		duty, done := runtime.nextDuty(channel, now)
		if duty != channel.Duty || channel.Mode != runtime.Mode {
			if err := s.writePWM(channel, duty, runtime.Mode); err != nil {
				log.Printf("[造浪] target=%s 写 PWM 失败: %v", targetID, err)
				channel.Status = "pwm_error"
				continue
			}
		}
		if done {
			runtime.Completed = true
		}
	}
}

func (r *pwmRuntime) nextDuty(channel *runtimeChannel, now time.Time) (int, bool) {
	elapsed := now.Sub(r.StartedAt)
	switch r.Mode {
	case "linearRamp":
		fromDuty := intValue(r.Params, "from", channel.Duty)
		toDuty := intValue(r.Params, "to", channel.Duty)
		durationMs := maxInt(intValue(r.Params, "durationMs", 1000), 1)
		loop := loopCountValue(r.Params)
		progress := float64(elapsed.Milliseconds()) / float64(durationMs)
		if loop != 0 {
			cycle := math.Mod(progress, 2)
			if cycle > 1 {
				progress = 2 - cycle
			} else {
				progress = cycle
			}
		}
		progress = clampFloat(progress, 0, 1)
		duty := roundInt(float64(fromDuty) + float64(toDuty-fromDuty)*progress)
		if loop < 0 {
			return duty, false
		}
		return duty, elapsed.Milliseconds() >= int64(durationMs*(loop+1))
	case "curveWave":
		fromDuty := intValue(r.Params, "from", channel.Duty)
		toDuty := intValue(r.Params, "to", channel.Duty)
		control1 := intValue(r.Params, "control1", toDuty)
		control2 := intValue(r.Params, "control2", fromDuty)
		durationMs := maxInt(intValue(r.Params, "durationMs", 3000), 1)
		curve := stringValue(r.Params, "curve", "linear")
		direction := directionValue(r.Params, "once")
		loop := loopCountValue(r.Params)
		phase := float64(elapsed.Milliseconds()) / float64(durationMs)
		cycleSpan := 1.0
		if direction == "pingpong" {
			cycleSpan = 2
		}
		local := phase
		if loop != 0 {
			local = math.Mod(phase, cycleSpan)
		}
		if direction == "pingpong" {
			if local > 1 {
				local = 2 - local
			}
		} else {
			local = clampFloat(local, 0, 1)
		}
		eased := curveProgress(local, curve, fromDuty, control1, control2, toDuty)
		duty := roundInt(float64(fromDuty) + float64(toDuty-fromDuty)*eased)
		if loop < 0 {
			return duty, false
		}
		return duty, phase >= cycleSpan*float64(loop+1)
	case "sineWave":
		minDuty := intValue(r.Params, "minDuty", 0)
		maxDuty := intValue(r.Params, "maxDuty", 1000)
		periodMs := maxInt(intValue(r.Params, "periodMs", 2500), 1)
		loop := loopCountValue(r.Params)
		progress := float64(elapsed.Milliseconds()) / float64(periodMs)
		cycles := progress
		if loop == 0 && progress >= 1 {
			cycles = 1
		}
		phase := cycles * 2 * math.Pi
		value := (math.Sin(phase-math.Pi/2) + 1) / 2
		duty := roundInt(float64(minDuty) + float64(maxDuty-minDuty)*value)
		if loop < 0 {
			return duty, false
		}
		return duty, progress >= float64(loop+1)
	case "bezierWave":
		fromDuty := intValue(r.Params, "from", channel.Duty)
		toDuty := intValue(r.Params, "to", channel.Duty)
		control1 := intValue(r.Params, "control1", toDuty)
		control2 := intValue(r.Params, "control2", fromDuty)
		durationMs := maxInt(intValue(r.Params, "durationMs", 3000), 1)
		loop := loopCountValue(r.Params)
		progress := float64(elapsed.Milliseconds()) / float64(durationMs)
		if loop != 0 {
			progress = progress - math.Floor(progress)
		} else {
			progress = clampFloat(progress, 0, 1)
		}
		value := cubicBezier(progress, float64(fromDuty), float64(control1), float64(control2), float64(toDuty))
		if loop < 0 {
			return roundInt(value), false
		}
		return roundInt(value), elapsed.Milliseconds() >= int64(durationMs*(loop+1))
	case "randomWave":
		minDuty := intValue(r.Params, "minDuty", 0)
		maxDuty := intValue(r.Params, "maxDuty", 1000)
		intervalMs := maxInt(intValue(r.Params, "intervalMs", 1200), 1)
		smoothing := clampFloat(float64(intValue(r.Params, "smoothing", 35))/100, 0.01, 1)
		loop := loopCountValue(r.Params)
		if r.LastRunAt.IsZero() {
			r.LastRunAt = now
			r.RandomDuty = minDuty
			r.CurrentDuty = float64(channel.Duty)
		}
		if now.Sub(r.LastRunAt) >= time.Duration(intervalMs)*time.Millisecond {
			r.LastRunAt = now
			r.RandomDuty = r.nextRandomDuty(minDuty, maxDuty)
		}
		r.CurrentDuty += (float64(r.RandomDuty) - r.CurrentDuty) * smoothing
		if loop < 0 {
			return roundInt(r.CurrentDuty), false
		}
		done := elapsed.Milliseconds() >= int64(intervalMs*(loop+1))
		return roundInt(r.CurrentDuty), done
	case "pulseWave":
		lowDuty := intValue(r.Params, "lowDuty", 0)
		highDuty := intValue(r.Params, "highDuty", 1000)
		onDurationMs := maxInt(intValue(r.Params, "onDurationMs", 800), 1)
		offDurationMs := maxInt(intValue(r.Params, "offDurationMs", 1200), 1)
		loop := loopCountValue(r.Params)
		cycleMs := onDurationMs + offDurationMs
		totalMs := elapsed.Milliseconds()
		if loop == 0 && totalMs >= int64(cycleMs) {
			return lowDuty, true
		}
		if loop > 0 && totalMs >= int64(cycleMs*(loop+1)) {
			return lowDuty, true
		}
		phaseMs := int(totalMs % int64(cycleMs))
		if phaseMs < onDurationMs {
			return highDuty, false
		}
		return lowDuty, false
	default:
		return channel.Duty, true
	}
}

func (r *pwmRuntime) nextRandomDuty(minDuty int, maxDuty int) int {
	if maxDuty <= minDuty {
		return minDuty
	}
	if r.RandomSeed == 0 {
		r.RandomSeed = uint64(time.Now().UnixNano()) ^ 0x9e3779b97f4a7c15
	}
	r.RandomSeed ^= r.RandomSeed << 13
	r.RandomSeed ^= r.RandomSeed >> 7
	r.RandomSeed ^= r.RandomSeed << 17
	span := maxDuty - minDuty + 1
	return minDuty + int(r.RandomSeed%uint64(span))
}

func roundInt(value float64) int {
	return int(math.Round(value))
}

func maxInt(a int, b int) int {
	if a > b {
		return a
	}
	return b
}

func (s *simulator) commandFromTimer(targetID string, action timerAction) driverCommand {
	kind := s.kindForTarget(targetID)
	if kind == "relay" {
		if action.Mode == "toggle" || action.Function == "toggle" {
			return driverCommand{
				TargetID:  targetID,
				Kind:      "relay",
				Operation: "toggle",
			}
		}
		state := firstNonEmpty(action.State, map[string]string{"turnOn": "on", "turnOff": "off"}[action.Function])
		return driverCommand{
			TargetID:  targetID,
			Kind:      "relay",
			Operation: "switch",
			Params:    map[string]any{"state": firstNonEmpty(state, "off")},
		}
	}

	mode := firstNonEmpty(action.Mode, functionToMode(action.Function))
	params := map[string]any{}
	switch mode {
	case "direct":
		params["duty"] = action.Duty
	case "curveWave":
		params["from"] = action.From
		params["to"] = action.To
		params["control1"] = action.Control1
		params["control2"] = action.Control2
		params["durationMs"] = action.DurationMs
		params["curve"] = action.Curve
		params["direction"] = action.Direction
		params["loop"] = action.Loop
	case "linearRamp":
		params["from"] = action.From
		params["to"] = action.To
		params["durationMs"] = action.DurationMs
		params["curve"] = action.Curve
		params["loop"] = action.Loop
	case "sineWave":
		params["minDuty"] = action.MinDuty
		params["maxDuty"] = action.MaxDuty
		params["periodMs"] = action.PeriodMs
		params["loop"] = action.Loop
	case "bezierWave":
		params["from"] = action.From
		params["to"] = action.To
		params["control1"] = action.Control1
		params["control2"] = action.Control2
		params["durationMs"] = action.DurationMs
		params["loop"] = action.Loop
	case "randomWave":
		params["minDuty"] = action.MinDuty
		params["maxDuty"] = action.MaxDuty
		params["intervalMs"] = action.IntervalMs
		params["smoothing"] = action.Smoothing
		params["loop"] = action.Loop
	case "pulseWave":
		params["lowDuty"] = action.LowDuty
		params["highDuty"] = action.HighDuty
		params["onDurationMs"] = action.OnDurationMs
		params["offDurationMs"] = action.OffDurationMs
		params["loop"] = action.Loop
	case "script":
	case "maxPower":
		mode = "direct"
		params["duty"] = 1000
	case "stop":
	default:
		mode = "direct"
		params["duty"] = action.Duty
	}
	return driverCommand{
		TargetID:  targetID,
		Kind:      "mos_pwm",
		Operation: mode,
		Params:    params,
	}
}

func (s *simulator) kindForTarget(targetID string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if channel := s.channels[targetID]; channel != nil {
		return channel.Kind
	}
	return ""
}

func functionToMode(function string) string {
	switch function {
	case "stop":
		return "stop"
	case "maxPower":
		return "maxPower"
	case "gentleWave":
		return "sineWave"
	case "randomWave":
		return "randomWave"
	case "curveWave":
		return "curveWave"
	case "toggle":
		return "toggle"
	default:
		return ""
	}
}

func allowWeekday(runAt time.Time, daysOfWeek []int) bool {
	if len(daysOfWeek) == 0 {
		return true
	}
	weekday := int(runAt.Weekday())
	if weekday == 0 {
		weekday = 7
	}
	for _, item := range daysOfWeek {
		if item == weekday {
			return true
		}
	}
	return false
}

func (s *simulator) connectMQTT(ctx context.Context) error {
	brokerURL := s.resolveBrokerURL()
	if brokerURL == "" {
		return fmt.Errorf("无法推断 MQTT Broker 地址")
	}

	options := mqttclient.NewClientOptions()
	options.AddBroker(normalizeBrokerURL(brokerURL))
	options.SetClientID(s.config.ClientID)
	options.SetAutoReconnect(true)
	options.SetOnConnectHandler(func(client mqttclient.Client) {
		token := client.Subscribe(s.bootstrap.DownlinkTopic, 1, s.handleMQTTMessage)
		token.Wait()
		if token.Error() != nil {
			log.Printf("订阅下行主题失败: %v", token.Error())
			return
		}
		log.Printf("已订阅下行主题: %s", s.bootstrap.DownlinkTopic)
		if err := s.publishStateReport("mqtt-connected"); err != nil {
			log.Printf("MQTT 连接后上报状态失败: %v", err)
		}
	})

	client := mqttclient.NewClient(options)
	token := client.Connect()
	token.Wait()
	if token.Error() != nil {
		return token.Error()
	}
	s.mqttClient = client

	go func() {
		<-ctx.Done()
		s.close()
	}()
	return nil
}

func normalizeBrokerURL(raw string) string {
	if raw == "" {
		return ""
	}
	if strings.HasPrefix(raw, "tcp://") || strings.HasPrefix(raw, "ssl://") || strings.HasPrefix(raw, "ws://") || strings.HasPrefix(raw, "wss://") {
		return raw
	}
	return "tcp://" + raw
}

func (s *simulator) resolveBrokerURL() string {
	if s.config.MQTTBroker != "" {
		return s.config.MQTTBroker
	}
	if s.bootstrap.Device.Transport.BrokerURL != "" {
		return s.bootstrap.Device.Transport.BrokerURL
	}
	parsed, err := url.Parse(s.config.APIBaseURL)
	if err != nil {
		return ""
	}
	host := parsed.Hostname()
	if host == "" {
		return ""
	}
	return host + ":1883"
}

func (s *simulator) handleMQTTMessage(_ mqttclient.Client, message mqttclient.Message) {
	var payload plannedPayload
	if err := json.Unmarshal(message.Payload(), &payload); err != nil {
		log.Printf("解析下行消息失败: %v", err)
		return
	}
	log.Printf("收到命令 topic=%s kind=%s operation=%s target=%s", message.Topic(), payload.Command.Kind, payload.Command.Operation, payload.Command.TargetID)
	if err := s.applyCommand(payload.Command); err != nil {
		log.Printf("执行命令失败: %v", err)
	}
	if err := s.publishStateReport("command"); err != nil {
		log.Printf("命令后状态上报失败: %v", err)
	}
}

func (s *simulator) applyCommand(command driverCommand) error {
	if command.Kind == "system" {
		switch command.Operation {
		case "reportState":
			log.Printf("[系统] 触发状态查询")
			return nil
		case "bootstrapRefresh":
			log.Printf("[系统] 触发 bootstrap 刷新")
			if err := s.fetchBootstrap(); err != nil {
				return err
			}
			s.initializeChannels()
			return nil
		default:
			log.Printf("[忽略] 未支持的系统命令 operation=%s", command.Operation)
			return nil
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	switch command.Kind {
	case "relay":
		return s.applyRelayCommand(command)
	case "mos_pwm":
		return s.applyPWMCommand(command)
	case "virtual_group":
		return s.applyGroupCommand(command)
	default:
		log.Printf("[忽略] 未支持的命令 kind=%s operation=%s", command.Kind, command.Operation)
		return nil
	}
}

func (s *simulator) applyRelayCommand(command driverCommand) error {
	channel := s.channels[command.TargetID]
	if channel == nil {
		return fmt.Errorf("未知继电器通道: %s", command.TargetID)
	}
	switch command.Operation {
	case "switch":
		if err := s.writeRelay(channel, stringValue(command.Params, "state", "off"), "switch"); err != nil {
			return err
		}
		log.Printf("[继电器] %s -> %s", command.TargetID, channel.State)
	case "toggle":
		nextState := "on"
		if channel.State == "on" {
			nextState = "off"
		}
		if err := s.writeRelay(channel, nextState, "toggle"); err != nil {
			return err
		}
		log.Printf("[继电器] %s toggle -> %s", command.TargetID, channel.State)
	default:
		log.Printf("[继电器] 未支持操作 %s", command.Operation)
	}
	return nil
}

func (s *simulator) applyPWMCommand(command driverCommand) error {
	channel := s.channels[command.TargetID]
	if channel == nil {
		return fmt.Errorf("未知 PWM 通道: %s", command.TargetID)
	}
	switch command.Operation {
	case "direct":
		delete(s.pwmRuntimes, command.TargetID)
		if err := s.writePWM(channel, intValue(command.Params, "duty", 0), "direct"); err != nil {
			return err
		}
		log.Printf("[造浪] %s 定速 duty=%d", command.TargetID, channel.Duty)
	case "curveWave":
		fromDuty := intValue(command.Params, "from", channel.Duty)
		toDuty := intValue(command.Params, "to", channel.Duty)
		durationMs := intValue(command.Params, "durationMs", 3000)
		s.pwmRuntimes[command.TargetID] = &pwmRuntime{
			Mode:      "curveWave",
			Params:    cloneParams(command.Params),
			StartedAt: time.Now().UTC(),
		}
		if err := s.writePWM(channel, fromDuty, "curveWave"); err != nil {
			return err
		}
		log.Printf("[造浪] %s 曲线波 %d -> %d duration=%dms curve=%s direction=%s loop=%d",
			command.TargetID,
			fromDuty,
			toDuty,
			durationMs,
			stringValue(command.Params, "curve", "linear"),
			directionValue(command.Params, "once"),
			loopCountValue(command.Params),
		)
	case "linearRamp":
		fromDuty := intValue(command.Params, "from", channel.Duty)
		toDuty := intValue(command.Params, "to", channel.Duty)
		durationMs := intValue(command.Params, "durationMs", 1000)
		s.pwmRuntimes[command.TargetID] = &pwmRuntime{
			Mode:      "linearRamp",
			Params:    cloneParams(command.Params),
			StartedAt: time.Now().UTC(),
		}
		if err := s.writePWM(channel, fromDuty, "linearRamp"); err != nil {
			return err
		}
		log.Printf("[造浪] %s 线性变速 %d -> %d duration=%dms loop=%d", command.TargetID, fromDuty, toDuty, durationMs, loopCountValue(command.Params))
	case "sineWave":
		s.pwmRuntimes[command.TargetID] = &pwmRuntime{
			Mode:      "sineWave",
			Params:    cloneParams(command.Params),
			StartedAt: time.Now().UTC(),
		}
		if err := s.writePWM(channel, intValue(command.Params, "minDuty", channel.Duty), "sineWave"); err != nil {
			return err
		}
		log.Printf("[造浪] %s 正弦波 min=%d max=%d period=%dms loop=%d",
			command.TargetID,
			intValue(command.Params, "minDuty", 0),
			intValue(command.Params, "maxDuty", channel.Duty),
			intValue(command.Params, "periodMs", 2500),
			loopCountValue(command.Params),
		)
	case "bezierWave":
		s.pwmRuntimes[command.TargetID] = &pwmRuntime{
			Mode:      "bezierWave",
			Params:    cloneParams(command.Params),
			StartedAt: time.Now().UTC(),
		}
		if err := s.writePWM(channel, intValue(command.Params, "from", channel.Duty), "bezierWave"); err != nil {
			return err
		}
		log.Printf("[造浪] %s 贝塞尔波 from=%d c1=%d c2=%d to=%d duration=%dms loop=%d",
			command.TargetID,
			intValue(command.Params, "from", 0),
			intValue(command.Params, "control1", 0),
			intValue(command.Params, "control2", 0),
			intValue(command.Params, "to", channel.Duty),
			intValue(command.Params, "durationMs", 3000),
			loopCountValue(command.Params),
		)
	case "randomWave":
		s.pwmRuntimes[command.TargetID] = &pwmRuntime{
			Mode:        "randomWave",
			Params:      cloneParams(command.Params),
			StartedAt:   time.Now().UTC(),
			CurrentDuty: float64(channel.Duty),
		}
		if err := s.writePWM(channel, intValue(command.Params, "minDuty", channel.Duty), "randomWave"); err != nil {
			return err
		}
		log.Printf("[造浪] %s 随机波 min=%d max=%d interval=%dms smoothing=%d loop=%d",
			command.TargetID,
			intValue(command.Params, "minDuty", 0),
			intValue(command.Params, "maxDuty", channel.Duty),
			intValue(command.Params, "intervalMs", 1200),
			intValue(command.Params, "smoothing", 0),
			loopCountValue(command.Params),
		)
	case "pulseWave":
		s.pwmRuntimes[command.TargetID] = &pwmRuntime{
			Mode:      "pulseWave",
			Params:    cloneParams(command.Params),
			StartedAt: time.Now().UTC(),
		}
		if err := s.writePWM(channel, intValue(command.Params, "highDuty", channel.Duty), "pulseWave"); err != nil {
			return err
		}
		log.Printf("[造浪] %s 脉冲波 low=%d high=%d on=%dms off=%dms loop=%d",
			command.TargetID,
			intValue(command.Params, "lowDuty", 0),
			intValue(command.Params, "highDuty", channel.Duty),
			intValue(command.Params, "onDurationMs", 800),
			intValue(command.Params, "offDurationMs", 1200),
			loopCountValue(command.Params),
		)
	case "script":
		delete(s.pwmRuntimes, command.TargetID)
		if s.scripts[command.TargetID] == nil {
			return fmt.Errorf("PWM 通道 %s 未配置脚本", command.TargetID)
		}
		channel.Mode = "script"
		channel.Status = "ok"
		log.Printf("[造浪] %s 切换到脚本驱动", command.TargetID)
	case "stop":
		delete(s.pwmRuntimes, command.TargetID)
		if err := s.writePWM(channel, 0, "stop"); err != nil {
			return err
		}
		log.Printf("[造浪] %s 停止输出", command.TargetID)
	default:
		log.Printf("[造浪] 未支持操作 %s", command.Operation)
	}
	return nil
}

func (s *simulator) applyGroupCommand(command driverCommand) error {
	channels, ok := command.Params["channels"].([]any)
	if !ok {
		log.Printf("[分组] channels 参数为空")
		return nil
	}
	log.Printf("[分组] sequenceGroup channels=%d loop=%d", len(channels), loopCountValue(command.Params))
	for _, item := range channels {
		channelMap, ok := item.(map[string]any)
		if !ok {
			continue
		}
		targetID := stringAnyValue(channelMap, "targetId", "")
		channel := s.channels[targetID]
		if channel == nil {
			continue
		}
		steps := stepList(channelMap["steps"])
		if err := s.writePWM(channel, finalDutyFromSteps(steps, channel.Duty), "sequenceGroup"); err != nil {
			return err
		}
		log.Printf("[分组] %s finalDuty=%d steps=%d", targetID, channel.Duty, len(steps))
	}
	return nil
}

func (s *simulator) currentGPIOBackend() gpioBackend {
	if s.gpioBackend != nil {
		return s.gpioBackend
	}
	return consoleGPIOBackend{}
}

func (s *simulator) writePWM(channel *runtimeChannel, duty int, mode string) error {
	if channel == nil {
		return fmt.Errorf("pwm channel is nil")
	}
	if err := s.currentGPIOBackend().WritePWM(channel.TargetID, channel.GPIO, duty); err != nil {
		return err
	}
	channel.Duty = duty
	if mode != "" {
		channel.Mode = mode
	}
	channel.Status = "ok"
	return nil
}

func (s *simulator) writeRelay(channel *runtimeChannel, state string, mode string) error {
	if channel == nil {
		return fmt.Errorf("relay channel is nil")
	}
	if err := s.currentGPIOBackend().WriteRelay(channel.TargetID, channel.GPIO, state); err != nil {
		return err
	}
	channel.State = state
	if mode != "" {
		channel.Mode = mode
	}
	channel.Status = "ok"
	return nil
}

func cloneParams(input map[string]any) map[string]any {
	if len(input) == 0 {
		return map[string]any{}
	}
	result := make(map[string]any, len(input))
	for key, value := range input {
		result[key] = value
	}
	return result
}

func (s *simulator) startPeriodicReport(ctx context.Context) {
	if s.config.ReportIntervalSec <= 0 {
		return
	}
	s.reportTicker = time.NewTicker(time.Duration(s.config.ReportIntervalSec) * time.Second)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-s.reportTicker.C:
				s.updateSensorValues()
				if err := s.publishStateReport("periodic"); err != nil {
					log.Printf("周期状态上报失败: %v", err)
				}
			}
		}
	}()
}

func (s *simulator) updateSensorValues() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, channel := range s.channels {
		if channel.Kind != "sensor_temperature" {
			continue
		}
		seconds := time.Since(s.startedAt).Seconds()
		channel.TemperatureC = 25 + math.Sin(seconds/20)*1.8
		channel.Mode = "read"
	}
}

func (s *simulator) publishStateReport(reason string) error {
	report := s.buildStateReport()
	s.lastReport = report
	payload, err := json.Marshal(report)
	if err != nil {
		return err
	}
	log.Printf("上报状态 reason=%s channels=%d", reason, len(report.Channels))

	if s.mqttClient != nil && s.mqttClient.IsConnectionOpen() {
		token := s.mqttClient.Publish(s.bootstrap.UpstreamTopic, 1, false, payload)
		token.Wait()
		if token.Error() != nil {
			return token.Error()
		}
	}

	reportURL := fmt.Sprintf("%s/api/public/device-state/report?deviceId=%s&deviceToken=%s",
		strings.TrimRight(s.config.APIBaseURL, "/"),
		url.QueryEscape(s.config.DeviceID),
		url.QueryEscape(s.config.DeviceToken),
	)
	request, err := http.NewRequest(http.MethodPost, reportURL, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := s.httpClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return decodeHTTPError(response)
	}
	return s.saveState()
}

func (s *simulator) buildStateReport() deviceStateReport {
	s.mu.Lock()
	defer s.mu.Unlock()

	channels := make(map[string]channelStateReport, len(s.channels))
	for targetID, channel := range s.channels {
		item := channelStateReport{
			TargetID:  targetID,
			Kind:      channel.Kind,
			Status:    firstNonEmpty(channel.Status, "ok"),
			UpdatedAt: time.Now().UTC().Format(time.RFC3339),
		}
		switch channel.Kind {
		case "relay":
			item.State = channel.State
			item.Mode = channel.Mode
		case "mos_pwm":
			item.Duty = channel.Duty
			item.Mode = channel.Mode
		case "sensor_temperature":
			item.TemperatureC = channel.TemperatureC
			item.Mode = channel.Mode
		}
		channels[targetID] = item
	}

	return deviceStateReport{
		DeviceID:   s.config.DeviceID,
		Online:     true,
		Ip:         "127.0.0.1",
		Rssi:       0,
		UptimeMs:   uint64(time.Since(s.startedAt).Milliseconds()),
		Channels:   channels,
		Metrics:    cloneMetrics(s.metrics),
		LastError:  "",
		ReportedAt: time.Now().UTC().Format(time.RFC3339),
	}
}

func (s *simulator) logBootstrapSummary() {
	log.Printf("设备仿真端启动 deviceId=%s platform=%s", s.config.DeviceID, s.config.Platform)
	log.Printf("控制器名称=%s 下行主题=%s 上行主题=%s", s.bootstrap.Device.Name, s.bootstrap.DownlinkTopic, s.bootstrap.UpstreamTopic)
	if len(s.bootstrap.DriverInstances) == 0 {
		log.Printf("当前没有驱动实例，可在用户端继续添加功能模块")
		return
	}
	for _, item := range s.bootstrap.DriverInstances {
		scriptState := "no-script"
		if source, ok := item.Config["scriptSource"].(string); ok && strings.TrimSpace(source) != "" {
			scriptState = "script-saved"
		}
		log.Printf("驱动实例 id=%s target=%s def=%s code=%s", item.ID, item.TargetID, item.DriverDefinitionID, scriptState)
	}
}

func (s *simulator) hasSavedBootstrap() bool {
	if s.bootstrap.Device.DeviceID != "" {
		return true
	}
	return len(s.bootstrap.DriverInstances) > 0 || len(s.bootstrap.Device.Capabilities) > 0
}

func (s *simulator) saveConfig() error {
	data, err := json.MarshalIndent(s.config, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(s.configPath)
	if dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	return os.WriteFile(s.configPath, data, 0o644)
}

func (s *simulator) loadSavedState() error {
	if s.statePath == "" {
		return nil
	}
	data, err := os.ReadFile(s.statePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var state simulatorState
	if err := json.Unmarshal(data, &state); err != nil {
		return err
	}
	if state.StateVersion == 0 {
		state.StateVersion = 1
	}
	if state.Bootstrap.Device.DeviceID == "" && len(state.Bootstrap.DriverInstances) == 0 {
		return nil
	}
	s.bootstrap = state.Bootstrap
	s.lastReport = state.LastReport
	log.Printf("已加载本地仿真状态: drivers=%d savedAt=%s", len(state.Bootstrap.DriverInstances), state.SavedAt)
	return nil
}

func (s *simulator) saveState() error {
	if s.statePath == "" {
		return nil
	}
	state := simulatorState{
		StateVersion: 1,
		Bootstrap:    s.bootstrap,
		LastReport:   s.lastReport,
		SavedAt:      time.Now().UTC().Format(time.RFC3339),
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(s.statePath)
	if dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	return os.WriteFile(s.statePath, data, 0o644)
}

func (s *simulator) close() {
	if s.timerTicker != nil {
		s.timerTicker.Stop()
	}
	if s.reportTicker != nil {
		s.reportTicker.Stop()
	}
	if s.mqttClient != nil && s.mqttClient.IsConnectionOpen() {
		s.mqttClient.Disconnect(250)
	}
}

func (s *simulator) runConsole(ctx context.Context) {
	scanner := bufio.NewScanner(os.Stdin)
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		if !scanner.Scan() {
			return
		}
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if err := s.executeConsoleCommand(line); err != nil {
			log.Printf("[控制台] %v", err)
		}
	}
}

func (s *simulator) executeConsoleCommand(line string) error {
	parts := strings.Fields(line)
	if len(parts) == 0 {
		return nil
	}
	switch parts[0] {
	case "help":
		s.printConsoleHelp()
	case "status":
		s.printChannelStatus()
	case "timers":
		s.printTimers()
	case "report":
		return s.publishStateReport("console")
	case "relay":
		if len(parts) < 3 {
			return fmt.Errorf("用法: relay <targetId> <on|off|toggle>")
		}
		command := driverCommand{TargetID: parts[1], Kind: "relay"}
		if parts[2] == "toggle" {
			command.Operation = "toggle"
		} else {
			command.Operation = "switch"
			command.Params = map[string]any{"state": parts[2]}
		}
		if err := s.applyCommand(command); err != nil {
			return err
		}
		return s.publishStateReport("console-relay")
	case "pwm":
		if len(parts) < 3 {
			return fmt.Errorf("用法: pwm <targetId> <duty>")
		}
		command := driverCommand{
			TargetID:  parts[1],
			Kind:      "mos_pwm",
			Operation: "direct",
			Params: map[string]any{
				"duty": parseInt(parts[2], 0),
			},
		}
		if err := s.applyCommand(command); err != nil {
			return err
		}
		return s.publishStateReport("console-pwm")
	case "wave":
		if len(parts) < 3 {
			return fmt.Errorf("用法: wave <targetId> <direct|curveWave|linearRamp|sineWave|bezierWave|randomWave|pulseWave|stop>")
		}
		command, err := buildWaveConsoleCommand(parts)
		if err != nil {
			return err
		}
		if err := s.applyCommand(command); err != nil {
			return err
		}
		return s.publishStateReport("console-wave")
	case "temp":
		if len(parts) < 3 {
			return fmt.Errorf("用法: temp <targetId> <value>")
		}
		return s.setTemperature(parts[1], parseFloat(parts[2], 25))
	case "command":
		if len(parts) < 2 {
			return fmt.Errorf("用法: command <json>")
		}
		var command driverCommand
		if err := json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "command "))), &command); err != nil {
			return fmt.Errorf("命令 JSON 非法: %w", err)
		}
		if err := s.applyCommand(command); err != nil {
			return err
		}
		return s.publishStateReport("console-command")
	case "exit", "quit":
		log.Println("[控制台] 请使用 Ctrl+C 结束进程")
	default:
		return fmt.Errorf("未知命令: %s", parts[0])
	}
	return nil
}

func buildWaveConsoleCommand(parts []string) (driverCommand, error) {
	targetID := parts[1]
	mode := parts[2]
	command := driverCommand{
		TargetID:  targetID,
		Kind:      "mos_pwm",
		Operation: mode,
		Params:    map[string]any{},
	}

	switch mode {
	case "direct":
		command.Params["duty"] = 700
		if len(parts) >= 4 {
			command.Params["duty"] = parseInt(parts[3], 700)
		}
	case "curveWave":
		command.Params["from"] = 220
		command.Params["to"] = 820
		command.Params["durationMs"] = 3000
		command.Params["curve"] = "easeInOut"
		command.Params["direction"] = "pingpong"
		command.Params["loop"] = -1
	case "linearRamp":
		command.Params["from"] = 220
		command.Params["to"] = 820
		command.Params["durationMs"] = 3000
	case "sineWave":
		command.Params["minDuty"] = 240
		command.Params["maxDuty"] = 820
		command.Params["periodMs"] = 2500
		command.Params["loop"] = -1
	case "bezierWave":
		command.Params["from"] = 220
		command.Params["to"] = 820
		command.Params["control1"] = 760
		command.Params["control2"] = 320
		command.Params["durationMs"] = 3000
		command.Params["loop"] = -1
	case "randomWave":
		command.Params["minDuty"] = 260
		command.Params["maxDuty"] = 880
		command.Params["intervalMs"] = 1200
		command.Params["smoothing"] = 35
		command.Params["loop"] = -1
	case "pulseWave":
		command.Params["lowDuty"] = 260
		command.Params["highDuty"] = 860
		command.Params["onDurationMs"] = 800
		command.Params["offDurationMs"] = 1200
		command.Params["loop"] = -1
	case "stop":
	default:
		return driverCommand{}, fmt.Errorf("不支持的造浪模式: %s", mode)
	}
	return command, nil
}

func (s *simulator) setTemperature(targetID string, value float64) error {
	s.mu.Lock()
	channel := s.channels[targetID]
	if channel == nil {
		s.mu.Unlock()
		return fmt.Errorf("未知温度通道: %s", targetID)
	}
	if channel.Kind != "sensor_temperature" {
		s.mu.Unlock()
		return fmt.Errorf("通道 %s 不是温度传感器", targetID)
	}
	channel.TemperatureC = value
	channel.Mode = "read"
	s.mu.Unlock()
	log.Printf("[温度] %s -> %.2f°C", targetID, value)
	return s.publishStateReport("console-temp")
}

func (s *simulator) printChannelStatus() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.channels) == 0 {
		log.Printf("[控制台] 当前没有已加载通道")
		return
	}
	for _, channel := range s.channels {
		switch channel.Kind {
		case "relay":
			log.Printf("[状态] relay target=%s state=%s mode=%s", channel.TargetID, channel.State, channel.Mode)
		case "mos_pwm":
			log.Printf("[状态] wave target=%s duty=%d mode=%s", channel.TargetID, channel.Duty, channel.Mode)
		case "sensor_temperature":
			log.Printf("[状态] temp target=%s value=%.2f mode=%s", channel.TargetID, channel.TemperatureC, channel.Mode)
		default:
			log.Printf("[状态] target=%s kind=%s", channel.TargetID, channel.Kind)
		}
	}
}

func (s *simulator) printConsoleHelp() {
	log.Println("[控制台] 可用命令:")
	log.Println("[控制台]   help")
	log.Println("[控制台]   status")
	log.Println("[控制台]   timers")
	log.Println("[控制台]   report")
	log.Println("[控制台]   relay <targetId> <on|off|toggle>")
	log.Println("[控制台]   pwm <targetId> <duty>")
	log.Println("[控制台]   wave <targetId> <direct|curveWave|linearRamp|sineWave|bezierWave|randomWave|pulseWave|stop>")
	log.Println("[控制台]   temp <targetId> <value>")
	log.Println("[控制台]   command <json>")
	log.Println("[控制台]   quit")
}

func (s *simulator) prepareConfig() error {
	changed := false
	if s.config.DeviceID == "" {
		s.config.DeviceID = randomDeviceID()
		changed = true
	}
	if s.config.Platform == "" {
		s.config.Platform = "esp32"
		changed = true
	}
	if s.config.ReportIntervalSec <= 0 {
		s.config.ReportIntervalSec = 30
		changed = true
	}
	if s.config.ClientID == "" {
		s.config.ClientID = "device-sim-" + s.config.DeviceID
		changed = true
	}
	if s.config.ConfigVersion == 0 {
		s.config.ConfigVersion = 1
		changed = true
	}
	if s.config.APIBaseURL == "" {
		s.config.APIBaseURL = "http://localhost:8080"
		changed = true
	}

	if changed {
		return s.saveConfig()
	}
	return nil
}

func loadSimulatorConfig(configPath string) (simulatorConfig, error) {
	defaultConfig := simulatorConfig{
		APIBaseURL:        "http://localhost:8080",
		DeviceID:          randomDeviceID(),
		Platform:          "esp32",
		ReportIntervalSec: 30,
		ConfigVersion:     1,
	}
	defaultConfig.ClientID = "device-sim-" + defaultConfig.DeviceID

	data, err := os.ReadFile(configPath)
	if err != nil {
		if os.IsNotExist(err) {
			if err := saveInitialConfig(configPath, defaultConfig); err != nil {
				return simulatorConfig{}, err
			}
			return defaultConfig, nil
		}
		return simulatorConfig{}, err
	}

	config := defaultConfig
	if err := json.Unmarshal(data, &config); err != nil {
		return simulatorConfig{}, err
	}
	if config.DeviceID == "" {
		config.DeviceID = defaultConfig.DeviceID
	}
	if config.Platform == "" {
		config.Platform = "esp32"
	}
	if config.ReportIntervalSec <= 0 {
		config.ReportIntervalSec = 30
	}
	if config.ClientID == "" {
		config.ClientID = "device-sim-" + config.DeviceID
	}
	if config.APIBaseURL == "" {
		config.APIBaseURL = "http://localhost:8080"
	}
	if config.ConfigVersion == 0 {
		config.ConfigVersion = 1
	}
	return config, nil
}

func saveInitialConfig(configPath string, config simulatorConfig) error {
	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(configPath, data, 0o644)
}

func promptLine(label string, defaultValue string) (string, error) {
	reader := bufio.NewReader(os.Stdin)
	if defaultValue != "" {
		fmt.Printf("%s [%s]: ", label, defaultValue)
	} else {
		fmt.Printf("%s: ", label)
	}
	line, err := reader.ReadString('\n')
	if err != nil {
		return "", err
	}
	line = strings.TrimSpace(line)
	if line == "" {
		return defaultValue, nil
	}
	return line, nil
}

func randomDeviceID() string {
	const alphabet = "23456789ABCDEFGHJKLMNPQRSTUVWXYZ"
	buffer := make([]byte, 6)
	randomBytes := make([]byte, len(buffer))
	_, _ = rand.Read(randomBytes)
	for index := range buffer {
		buffer[index] = alphabet[int(randomBytes[index])%len(alphabet)]
	}
	return "sim-" + string(buffer)
}

func (s *simulator) printTimers() {
	if len(s.bootstrap.Device.TimerGroups) == 0 {
		log.Printf("[定时器] 当前没有定时器")
		return
	}
	for _, group := range s.bootstrap.Device.TimerGroups {
		for _, timer := range group.Timers {
			log.Printf("[定时器] target=%s id=%s enabled=%v at=%s tz=UTC days=%v mode=%s function=%s",
				group.TargetID, timer.ID, timer.Enabled, timer.At, timer.DaysOfWeek, timer.Action.Mode, timer.Action.Function)
		}
	}
}

func decodeHTTPError(response *http.Response) error {
	var payload map[string]any
	if err := json.NewDecoder(response.Body).Decode(&payload); err == nil {
		if message, ok := payload["error"].(string); ok && message != "" {
			return fmt.Errorf("HTTP %d: %s", response.StatusCode, message)
		}
	}
	return fmt.Errorf("HTTP %d", response.StatusCode)
}

func cloneMetrics(values map[string]float64) map[string]float64 {
	if len(values) == 0 {
		return nil
	}
	result := make(map[string]float64, len(values))
	for key, value := range values {
		result[key] = value
	}
	return result
}

func stringValue(values map[string]any, key string, fallback string) string {
	if values == nil {
		return fallback
	}
	value, ok := values[key].(string)
	if !ok || value == "" {
		return fallback
	}
	return value
}

func stringAnyValue(values map[string]any, key string, fallback string) string {
	value, ok := values[key].(string)
	if !ok || value == "" {
		return fallback
	}
	return value
}

func intValue(values map[string]any, key string, fallback int) int {
	if values == nil {
		return fallback
	}
	switch value := values[key].(type) {
	case int:
		return value
	case int32:
		return int(value)
	case int64:
		return int(value)
	case float64:
		return int(value)
	default:
		return fallback
	}
}

func boolValue(values map[string]any, key string, fallback bool) bool {
	if values == nil {
		return fallback
	}
	value, ok := values[key].(bool)
	if !ok {
		return fallback
	}
	return value
}

func stepList(raw any) []map[string]any {
	items, ok := raw.([]any)
	if !ok {
		return nil
	}
	result := make([]map[string]any, 0, len(items))
	for _, item := range items {
		step, ok := item.(map[string]any)
		if !ok {
			continue
		}
		result = append(result, step)
	}
	return result
}

func finalDutyFromSteps(steps []map[string]any, fallback int) int {
	if len(steps) == 0 {
		return fallback
	}
	last := steps[len(steps)-1]
	switch value := last["duty"].(type) {
	case int:
		return value
	case int32:
		return int(value)
	case int64:
		return int(value)
	case float64:
		return int(value)
	default:
		return fallback
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func parseInt(value string, fallback int) int {
	var parsed int
	if _, err := fmt.Sscanf(value, "%d", &parsed); err != nil {
		return fallback
	}
	return parsed
}

func parseFloat(value string, fallback float64) float64 {
	var parsed float64
	if _, err := fmt.Sscanf(value, "%f", &parsed); err != nil {
		return fallback
	}
	return parsed
}
