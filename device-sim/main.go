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
	State        string
	Duty         int
	Mode         string
	TemperatureC float64
	Status       string
}

type transportConfig struct {
	Protocol    string `json:"protocol"`
	TopicPrefix string `json:"topicPrefix"`
	BrokerURL   string `json:"brokerUrl,omitempty"`
}

type capability struct {
	ID           string `json:"id"`
	Kind         string `json:"kind"`
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
	Function   string            `json:"function,omitempty"`
	Mode       string            `json:"mode"`
	State      string            `json:"state,omitempty"`
	Duty       int               `json:"duty,omitempty"`
	From       int               `json:"from,omitempty"`
	To         int               `json:"to,omitempty"`
	MinDuty    int               `json:"minDuty,omitempty"`
	MaxDuty    int               `json:"maxDuty,omitempty"`
	LowDuty    int               `json:"lowDuty,omitempty"`
	HighDuty   int               `json:"highDuty,omitempty"`
	Control1   int               `json:"control1,omitempty"`
	Control2   int               `json:"control2,omitempty"`
	DurationMs int               `json:"durationMs,omitempty"`
	IntervalMs int               `json:"intervalMs,omitempty"`
	PeriodMs   int               `json:"periodMs,omitempty"`
	OnDurationMs int             `json:"onDurationMs,omitempty"`
	OffDurationMs int            `json:"offDurationMs,omitempty"`
	Repeat     int               `json:"repeat,omitempty"`
	Smoothing  int               `json:"smoothing,omitempty"`
	Curve      string            `json:"curve,omitempty"`
	Loop       bool              `json:"loop,omitempty"`
	Steps      []sequenceStep    `json:"steps,omitempty"`
	Channels   []sequenceChannel `json:"channels,omitempty"`
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
	LastError  string                        `json:"lastError,omitempty"`
	ReportedAt string                        `json:"reportedAt,omitempty"`
}

type pairResponse struct {
	DeviceToken string           `json:"deviceToken"`
	Bootstrap   bootstrapPayload `json:"bootstrap"`
}

type simulator struct {
	config     simulatorConfig
	configPath string
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
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
		channels:     map[string]*runtimeChannel{},
		startedAt:    time.Now(),
		lastTimerRun: map[string]string{},
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	if err := deviceSimulator.bootstrapSession(ctx); err != nil {
		log.Fatalf("启动仿真端失败: %v", err)
	}
	defer deviceSimulator.close()

	go deviceSimulator.runConsole(ctx)

	<-ctx.Done()
	log.Println("收到退出信号，设备仿真端正在关闭")
}

func (s *simulator) bootstrapSession(ctx context.Context) error {
	if err := s.ensureInteractiveConfig(); err != nil {
		return err
	}

	var err error
	if s.config.DeviceToken == "" {
		err = s.pairDevice()
	} else {
		err = s.fetchBootstrap()
	}
	if err != nil {
		return err
	}

	s.initializeChannels()
	if err := s.connectMQTT(ctx); err != nil {
		return err
	}
	s.startTimerLoop(ctx)
	s.startPeriodicReport(ctx)
	s.logBootstrapSummary()
	s.printConsoleHelp()
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
	return s.saveConfig()
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
	return nil
}

func (s *simulator) initializeChannels() {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.channels = map[string]*runtimeChannel{}
	s.lastTimerRun = map[string]string{}
	capabilities := s.bootstrap.Device.Capabilities
	if len(s.bootstrap.DriverInstances) > 0 {
		capabilities = capabilitiesFromDriverInstances(s.bootstrap.DriverInstances)
	}
	for _, capability := range capabilities {
		channel := &runtimeChannel{
			TargetID: capability.ID,
			Kind:     capability.Kind,
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
}

func capabilitiesFromDriverInstances(items []driverInstance) []capability {
	result := make([]capability, 0, len(items))
	for _, item := range items {
		switch item.DriverDefinitionID {
		case "driver-relay-builtin":
			result = append(result, capability{
				ID:           item.TargetID,
				Kind:         "relay",
				DefaultState: stringValue(item.Config, "defaultPowerOnState", "off"),
			})
		case "driver-mos-pwm-builtin":
			result = append(result, capability{
				ID:   item.TargetID,
				Kind: "mos_pwm",
			})
		case "driver-ds18b20-builtin":
			result = append(result, capability{
				ID:   item.TargetID,
				Kind: "sensor_temperature",
			})
		}
	}
	return result
}

func (s *simulator) startTimerLoop(ctx context.Context) {
	s.timerTicker = time.NewTicker(time.Second)
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
	case "linearRamp":
		params["from"] = action.From
		params["to"] = action.To
		params["durationMs"] = action.DurationMs
		params["curve"] = action.Curve
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
	s.mu.Lock()
	defer s.mu.Unlock()

	if command.Kind == "system" && command.Operation == "reportState" {
		log.Printf("[系统] 触发状态查询")
		return nil
	}

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
		channel.State = stringValue(command.Params, "state", "off")
		channel.Mode = "switch"
		log.Printf("[继电器] %s -> %s", command.TargetID, channel.State)
	case "toggle":
		if channel.State == "on" {
			channel.State = "off"
		} else {
			channel.State = "on"
		}
		channel.Mode = "toggle"
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
		channel.Duty = intValue(command.Params, "duty", 0)
		channel.Mode = "direct"
		log.Printf("[造浪] %s 定速 duty=%d", command.TargetID, channel.Duty)
	case "linearRamp":
		fromDuty := intValue(command.Params, "from", channel.Duty)
		toDuty := intValue(command.Params, "to", channel.Duty)
		durationMs := intValue(command.Params, "durationMs", 1000)
		channel.Duty = toDuty
		channel.Mode = "linearRamp"
		log.Printf("[造浪] %s 线性变速 %d -> %d duration=%dms", command.TargetID, fromDuty, toDuty, durationMs)
	case "sineWave":
		channel.Duty = intValue(command.Params, "maxDuty", channel.Duty)
		channel.Mode = "sineWave"
		log.Printf("[造浪] %s 正弦波 min=%d max=%d period=%dms loop=%v",
			command.TargetID,
			intValue(command.Params, "minDuty", 0),
			intValue(command.Params, "maxDuty", channel.Duty),
			intValue(command.Params, "periodMs", 2500),
			boolValue(command.Params, "loop", false),
		)
	case "bezierWave":
		channel.Duty = intValue(command.Params, "to", channel.Duty)
		channel.Mode = "bezierWave"
		log.Printf("[造浪] %s 贝塞尔波 from=%d c1=%d c2=%d to=%d duration=%dms loop=%v",
			command.TargetID,
			intValue(command.Params, "from", 0),
			intValue(command.Params, "control1", 0),
			intValue(command.Params, "control2", 0),
			intValue(command.Params, "to", channel.Duty),
			intValue(command.Params, "durationMs", 3000),
			boolValue(command.Params, "loop", false),
		)
	case "randomWave":
		channel.Duty = intValue(command.Params, "maxDuty", channel.Duty)
		channel.Mode = "randomWave"
		log.Printf("[造浪] %s 随机波 min=%d max=%d interval=%dms smoothing=%d loop=%v",
			command.TargetID,
			intValue(command.Params, "minDuty", 0),
			intValue(command.Params, "maxDuty", channel.Duty),
			intValue(command.Params, "intervalMs", 1200),
			intValue(command.Params, "smoothing", 0),
			boolValue(command.Params, "loop", false),
		)
	case "pulseWave":
		channel.Duty = intValue(command.Params, "highDuty", channel.Duty)
		channel.Mode = "pulseWave"
		log.Printf("[造浪] %s 脉冲波 low=%d high=%d on=%dms off=%dms loop=%v",
			command.TargetID,
			intValue(command.Params, "lowDuty", 0),
			intValue(command.Params, "highDuty", channel.Duty),
			intValue(command.Params, "onDurationMs", 800),
			intValue(command.Params, "offDurationMs", 1200),
			boolValue(command.Params, "loop", false),
		)
	case "stop":
		channel.Duty = 0
		channel.Mode = "stop"
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
	log.Printf("[分组] sequenceGroup channels=%d loop=%v", len(channels), boolValue(command.Params, "loop", false))
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
		channel.Duty = finalDutyFromSteps(steps, channel.Duty)
		channel.Mode = "sequenceGroup"
		log.Printf("[分组] %s finalDuty=%d steps=%d", targetID, channel.Duty, len(steps))
	}
	return nil
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
	return nil
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
		log.Printf("驱动实例 id=%s target=%s def=%s", item.ID, item.TargetID, item.DriverDefinitionID)
	}
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
	case "bootstrap":
		if err := s.fetchBootstrap(); err != nil {
			return err
		}
		s.initializeChannels()
		log.Printf("[控制台] 已重新拉取 bootstrap")
		return s.publishStateReport("console-bootstrap")
	case "setup":
		oldToken := s.config.DeviceToken
		if err := s.setupWizard(true); err != nil {
			return err
		}
		if oldToken != s.config.DeviceToken {
			log.Printf("[控制台] 配置已更新，下次启动会按新设备状态处理")
			return nil
		}
		return s.publishStateReport("console-setup")
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
			return fmt.Errorf("用法: wave <targetId> <direct|linearRamp|sineWave|bezierWave|randomWave|pulseWave|stop>")
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
	case "linearRamp":
		command.Params["from"] = 220
		command.Params["to"] = 820
		command.Params["durationMs"] = 3000
	case "sineWave":
		command.Params["minDuty"] = 240
		command.Params["maxDuty"] = 820
		command.Params["periodMs"] = 2500
		command.Params["loop"] = true
	case "bezierWave":
		command.Params["from"] = 220
		command.Params["to"] = 820
		command.Params["control1"] = 760
		command.Params["control2"] = 320
		command.Params["durationMs"] = 3000
		command.Params["loop"] = true
	case "randomWave":
		command.Params["minDuty"] = 260
		command.Params["maxDuty"] = 880
		command.Params["intervalMs"] = 1200
		command.Params["smoothing"] = 35
		command.Params["loop"] = true
	case "pulseWave":
		command.Params["lowDuty"] = 260
		command.Params["highDuty"] = 860
		command.Params["onDurationMs"] = 800
		command.Params["offDurationMs"] = 1200
		command.Params["loop"] = true
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
	log.Println("[控制台]   setup")
	log.Println("[控制台]   status")
	log.Println("[控制台]   timers")
	log.Println("[控制台]   report")
	log.Println("[控制台]   bootstrap")
	log.Println("[控制台]   relay <targetId> <on|off|toggle>")
	log.Println("[控制台]   pwm <targetId> <duty>")
	log.Println("[控制台]   wave <targetId> <direct|linearRamp|sineWave|bezierWave|randomWave|pulseWave|stop>")
	log.Println("[控制台]   temp <targetId> <value>")
	log.Println("[控制台]   command <json>")
	log.Println("[控制台]   quit")
}

func (s *simulator) ensureInteractiveConfig() error {
	return s.setupWizard(false)
}

func (s *simulator) setupWizard(force bool) error {
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

	if force || s.config.APIBaseURL == "" {
		value, err := promptLine("请输入后端地址", firstNonEmpty(s.config.APIBaseURL, "http://localhost:8080"))
		if err != nil {
			return err
		}
		s.config.APIBaseURL = value
		changed = true
	}

	if force {
		deviceID, err := promptLine("请输入设备 ID", s.config.DeviceID)
		if err != nil {
			return err
		}
		if deviceID != s.config.DeviceID {
			s.config.DeviceID = deviceID
			s.config.ClientID = "device-sim-" + s.config.DeviceID
			s.config.DeviceToken = ""
			changed = true
		}

		platform, err := promptLine("请输入平台", firstNonEmpty(s.config.Platform, "esp32"))
		if err != nil {
			return err
		}
		if platform == "" {
			platform = "esp32"
		}
		if platform != s.config.Platform {
			s.config.Platform = platform
			s.config.DeviceToken = ""
			changed = true
		}
	}

	if changed {
		return s.saveConfig()
	}
	return nil
}

func loadSimulatorConfig(configPath string) (simulatorConfig, error) {
	defaultConfig := simulatorConfig{
		APIBaseURL:        "",
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
