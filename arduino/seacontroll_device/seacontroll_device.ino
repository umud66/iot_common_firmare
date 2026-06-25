#include "device_config.h"

#ifdef ESP32
#include <WiFi.h>
#include <HTTPClient.h>
#include <WebServer.h>
#include <EEPROM.h>
#include <time.h>
#else
#include <ESP8266WiFi.h>
#include <ESP8266HTTPClient.h>
#include <ESP8266WebServer.h>
#include <EEPROM.h>
#include <time.h>
#endif

#include <ArduinoJson.h>
#include <PubSubClient.h>

#define EEPROM_SIZE 1024

// 中文注释：设备配置持久化到 EEPROM，用户在 AP 配网页里填写
struct DeviceSettings {
  char magic[8];
  char wifiSsid[64];
  char wifiPassword[64];
  char apiBaseUrl[128];
  char deviceId[64];
  char linkCode[32];
  char deviceToken[64];
};

// 中文注释：通道类型定义，便于统一处理继电器与 PWM
enum ChannelKind {
  CHANNEL_RELAY,
  CHANNEL_MOS_PWM
};

struct PWMInfo {
  int channel;
  int frequency;
  int resolution;
  int minDuty;
  int maxDuty;
};

struct ChannelConfig {
  String id;
  ChannelKind kind;
  int gpio;
  String pinRole;
  String activeLevel;
  String defaultState;
  PWMInfo pwm;
  String currentState;
  int currentDuty;
  String currentMode;
};

struct TimerConfig {
  String id;
  String targetId;
  bool enabled;
  uint8_t hour;
  uint8_t minute;
  uint8_t daysMask;
  String mode;
  String functionKey;
  String state;
  int duty;
  int from;
  int to;
  int durationMs;
  int intervalMs;
  int repeat;
  bool loop;
};

WiFiClient wifiClient;
PubSubClient mqttClient(wifiClient);
DeviceSettings settings;

#ifdef ESP32
WebServer webServer(80);
#else
ESP8266WebServer webServer(80);
#endif

String downlinkTopic;
String upstreamTopic;
String mqttHost;
int mqttPort = DEFAULT_MQTT_PORT;
unsigned long lastStateReportAt = 0;
ChannelConfig channels[8];
size_t channelCount = 0;
TimerConfig timers[80];
size_t timerCount = 0;
time_t lastTimerCheckAt = 0;
bool apMode = false;

bool parseTimerTime(const String& value, uint8_t& hour, uint8_t& minute) {
  if (value.length() != 5 || value.charAt(2) != ':') {
    return false;
  }
  hour = value.substring(0, 2).toInt();
  minute = value.substring(3, 5).toInt();
  return hour < 24 && minute < 60;
}

uint8_t buildDaysMask(JsonArray daysOfWeek) {
  uint8_t mask = 0;
  for (JsonVariant dayVariant : daysOfWeek) {
    int day = dayVariant | 0;
    if (day >= 1 && day <= 7) {
      mask |= (1 << (day - 1));
    }
  }
  return mask;
}

bool shouldRunToday(const TimerConfig& timer, int weekday) {
  if (timer.daysMask == 0) {
    return true;
  }
  if (weekday < 1 || weekday > 7) {
    return false;
  }
  return (timer.daysMask & (1 << (weekday - 1))) != 0;
}

bool appendTimerFromJson(const String& targetId, JsonVariant timerVariant) {
  if (timerCount >= 80) {
    return false;
  }

  uint8_t hour = 0;
  uint8_t minute = 0;
  String at = timerVariant["at"] | "";
  if (!parseTimerTime(at, hour, minute)) {
    return false;
  }

  TimerConfig timer;
  timer.id = timerVariant["id"] | "";
  timer.targetId = targetId;
  timer.enabled = timerVariant["enabled"] | false;
  timer.hour = hour;
  timer.minute = minute;
  timer.daysMask = buildDaysMask(timerVariant["daysOfWeek"].as<JsonArray>());

  JsonObject action = timerVariant["action"];
  timer.mode = action["mode"] | "";
  timer.functionKey = action["function"] | "";
  timer.state = action["state"] | "";
  timer.duty = action["duty"] | 0;
  timer.from = action["from"] | 0;
  timer.to = action["to"] | 0;
  timer.durationMs = action["durationMs"] | 1000;
  timer.intervalMs = action["intervalMs"] | 1000;
  timer.repeat = action["repeat"] | 1;
  timer.loop = action["loop"] | false;
  timers[timerCount++] = timer;
  return true;
}

void loadTimersFromBootstrap(JsonObject deviceObject) {
  timerCount = 0;
  JsonArray timerGroups = deviceObject["timerGroups"].as<JsonArray>();
  for (JsonVariant groupVariant : timerGroups) {
    String targetId = groupVariant["targetId"] | "";
    JsonArray groupTimers = groupVariant["timers"].as<JsonArray>();
    for (JsonVariant timerVariant : groupTimers) {
      if (timerCount >= 80) {
        return;
      }
      appendTimerFromJson(targetId, timerVariant);
    }
  }
}

void syncDeviceTime() {
  setenv("TZ", "UTC0", 1);
  tzset();
  configTime(0, 0, "ntp.aliyun.com", "pool.ntp.org", "ntp1.aliyun.com");
  time_t now = time(nullptr);
  unsigned long startedAt = millis();
  while (now < 100000 && millis() - startedAt < 15000) {
    delay(500);
    now = time(nullptr);
  }
  if (now >= 100000) {
    Serial.println("time synced utc");
  } else {
    Serial.println("time sync timeout");
  }
}

void executeTimerAction(const TimerConfig& timer) {
  ChannelConfig* channel = findChannel(timer.targetId);
  if (channel == nullptr) {
    return;
  }

  if (channel->kind == CHANNEL_RELAY) {
    if (timer.mode == "toggle" || timer.functionKey == "toggle") {
      applyRelayToggle(timer.targetId);
      return;
    }
    String nextState = timer.state;
    if (nextState.length() == 0) {
      if (timer.functionKey == "turnOn") {
        nextState = "on";
      } else if (timer.functionKey == "turnOff") {
        nextState = "off";
      }
    }
    applyRelay(timer.targetId, nextState.length() == 0 ? "off" : nextState);
    return;
  }

  String operation = timer.mode;
  if (operation.length() == 0) {
    if (timer.functionKey == "softStart") {
      operation = "softStart";
    } else if (timer.functionKey == "softStop") {
      operation = "softStop";
    } else if (timer.functionKey == "pulse") {
      operation = "pulse";
    } else if (timer.functionKey == "stop") {
      operation = "stop";
    } else if (timer.functionKey == "maxPower") {
      operation = "direct";
    } else {
      operation = "direct";
    }
  }

  if (operation == "direct") {
    int duty = timer.functionKey == "maxPower" ? channel->pwm.maxDuty : timer.duty;
    applyDirectPWM(timer.targetId, duty);
  } else if (operation == "linearRamp") {
    applyLinearRamp(timer.targetId, timer.from, timer.to, timer.durationMs);
  } else if (operation == "softStart") {
    applySoftStart(timer.targetId, timer.to, timer.durationMs);
  } else if (operation == "softStop") {
    applySoftStop(timer.targetId, timer.from, timer.durationMs);
  } else if (operation == "pulse") {
    applyPulse(timer.targetId, timer.duty, timer.durationMs, timer.intervalMs, timer.repeat, timer.loop);
  } else if (operation == "stop") {
    applyDirectPWM(timer.targetId, 0);
    channel->currentMode = "stop";
  }
}

void runLocalTimers() {
  time_t now = time(nullptr);
  if (now < 100000) {
    return;
  }
  if (now == lastTimerCheckAt) {
    return;
  }
  lastTimerCheckAt = now;

  struct tm timeInfo;
  localtime_r(&now, &timeInfo);
  int weekday = timeInfo.tm_wday == 0 ? 7 : timeInfo.tm_wday;
  for (size_t i = 0; i < timerCount; i++) {
    TimerConfig& timer = timers[i];
    if (!timer.enabled) {
      continue;
    }
    if (!shouldRunToday(timer, weekday)) {
      continue;
    }
    if (timeInfo.tm_hour != timer.hour || timeInfo.tm_min != timer.minute || timeInfo.tm_sec != 0) {
      continue;
    }
    Serial.printf("timer triggered: %s -> %s\n", timer.id.c_str(), timer.targetId.c_str());
    executeTimerAction(timer);
    publishStateReport();
    lastStateReportAt = millis();
  }
}

int gpioFromBindings(JsonArray bindings, const String& pinRole) {
  for (JsonVariant binding : bindings) {
    String currentRole = binding["pinRole"] | "";
    if (pinRole == "" || currentRole == pinRole) {
      return binding["gpio"] | -1;
    }
  }
  return -1;
}

bool appendChannelFromCapability(JsonVariant capability) {
  if (channelCount >= 8) {
    return false;
  }

  String kindText = capability["kind"].as<String>();
  if (kindText == "virtual_group") {
    return false;
  }
  if (kindText != "relay" && kindText != "mos_pwm") {
    return false;
  }

  ChannelConfig config;
  config.id = capability["id"].as<String>();
  config.gpio = capability["gpio"] | -1;
  config.pinRole = kindText == "relay" ? "control" : "pwm";
  config.activeLevel = capability["activeLevel"] | "high";
  config.defaultState = capability["defaultState"] | "off";
  config.kind = kindText == "relay" ? CHANNEL_RELAY : CHANNEL_MOS_PWM;
  config.currentState = "off";
  config.currentDuty = 0;
  config.currentMode = "direct";
  if (config.kind == CHANNEL_MOS_PWM) {
    JsonObject pwm = capability["pwm"];
    config.pwm.channel = pwm["channel"] | 0;
    config.pwm.frequency = pwm["frequency"] | 20000;
    config.pwm.resolution = pwm["resolution"] | 1023;
    config.pwm.minDuty = pwm["minDuty"] | 0;
    config.pwm.maxDuty = pwm["maxDuty"] | 1023;
  }
  channels[channelCount++] = config;
  return true;
}

bool appendChannelFromDriverInstance(JsonVariant instance) {
  if (channelCount >= 8) {
    return false;
  }

  String driverDefinitionId = instance["driverDefinitionId"] | "";
  String kindText = "";
  String pinRole = "";
  if (driverDefinitionId == "driver-relay-builtin") {
    kindText = "relay";
    pinRole = "control";
  } else if (driverDefinitionId == "driver-mos-pwm-builtin") {
    kindText = "mos_pwm";
    pinRole = "pwm";
  } else {
    return false;
  }

  JsonObject configJson = instance["config"];
  JsonArray gpioBindings = instance["gpioBindings"].as<JsonArray>();

  ChannelConfig config;
  config.id = instance["targetId"].as<String>();
  config.gpio = gpioFromBindings(gpioBindings, pinRole);
  config.pinRole = pinRole;
  config.activeLevel = configJson["activeLevel"] | "high";
  config.defaultState = configJson["defaultPowerOnState"] | "off";
  config.kind = kindText == "relay" ? CHANNEL_RELAY : CHANNEL_MOS_PWM;
  config.currentState = "off";
  config.currentDuty = 0;
  config.currentMode = "direct";
  if (config.kind == CHANNEL_MOS_PWM) {
    config.pwm.channel = configJson["channel"] | 0;
    config.pwm.frequency = configJson["frequency"] | 20000;
    config.pwm.resolution = configJson["resolution"] | 1023;
    config.pwm.minDuty = configJson["minDuty"] | 0;
    config.pwm.maxDuty = configJson["maxDuty"] | 1023;
  }
  channels[channelCount++] = config;
  return true;
}

void setup() {
  Serial.begin(115200);
  delay(1000);

  EEPROM.begin(EEPROM_SIZE);
  loadSettings();

  if (!hasSavedWifi()) {
    startAccessPointMode();
    return;
  }

  if (!connectWiFi()) {
    startAccessPointMode();
    return;
  }

  syncDeviceTime();

  if (!ensureProvisioned()) {
    startAccessPointMode();
    return;
  }

  bootstrapFromServer();
  setupChannelPins();
  mqttClient.setServer(mqttHost.c_str(), mqttPort);
  mqttClient.setCallback(onMessage);
}

void loop() {
  if (apMode) {
    webServer.handleClient();
    delay(10);
    return;
  }

  ensureMqttConnected();
  mqttClient.loop();
  runLocalTimers();

  if (millis() - lastStateReportAt > 5000) {
    publishStateReport();
    lastStateReportAt = millis();
  }
}

void loadSettings() {
  EEPROM.get(0, settings);
  if (String(settings.magic) != "SEACTRL") {
    memset(&settings, 0, sizeof(settings));
    strcpy(settings.magic, "SEACTRL");
    strcpy(settings.deviceId, DEFAULT_DEVICE_ID);
    strcpy(settings.apiBaseUrl, DEFAULT_API_BASE_URL);
    saveSettings();
  }
}

void saveSettings() {
  EEPROM.put(0, settings);
  EEPROM.commit();
}

bool hasSavedWifi() {
  return strlen(settings.wifiSsid) > 0 && strlen(settings.apiBaseUrl) > 0;
}

bool connectWiFi() {
  WiFi.mode(WIFI_STA);
  WiFi.begin(settings.wifiSsid, settings.wifiPassword);

  unsigned long startedAt = millis();
  while (WiFi.status() != WL_CONNECTED && millis() - startedAt < 20000) {
    delay(500);
    Serial.print(".");
  }
  Serial.println();

  if (WiFi.status() != WL_CONNECTED) {
    Serial.println("WiFi connect failed");
    return false;
  }

  Serial.println("WiFi connected");
  mqttHost = extractHost(settings.apiBaseUrl);
  return true;
}

void startAccessPointMode() {
  apMode = true;
  WiFi.mode(WIFI_AP);
  WiFi.softAP("SeaControll-Setup");

  webServer.on("/", HTTP_GET, handlePortalIndex);
  webServer.on("/save", HTTP_POST, handlePortalSave);
  webServer.begin();

  Serial.println("AP mode ready: http://192.168.4.1");
}

void handlePortalIndex() {
  String html = "<!doctype html><html><head><meta charset='utf-8'><title>SeaControll 配网</title></head><body>";
  html += "<h1>SeaControll 设备配网</h1>";
  html += "<form method='POST' action='/save'>";
  html += "WiFi SSID：<br><input name='wifiSsid' value='" + String(settings.wifiSsid) + "'><br><br>";
  html += "WiFi 密码：<br><input name='wifiPassword' type='password' value='" + String(settings.wifiPassword) + "'><br><br>";
  html += "服务端地址：<br><input name='apiBaseUrl' value='" + String(settings.apiBaseUrl) + "'><br><br>";
  html += "设备 ID：<br><input name='deviceId' value='" + String(settings.deviceId) + "'><br><br>";
  html += "设备链接代码：<br><input name='linkCode' value='" + String(settings.linkCode) + "'><br><br>";
  html += "<button type='submit'>保存并重启</button>";
  html += "</form></body></html>";
  webServer.send(200, "text/html; charset=utf-8", html);
}

void handlePortalSave() {
  writeCString(settings.wifiSsid, webServer.arg("wifiSsid"), sizeof(settings.wifiSsid));
  writeCString(settings.wifiPassword, webServer.arg("wifiPassword"), sizeof(settings.wifiPassword));
  writeCString(settings.apiBaseUrl, webServer.arg("apiBaseUrl"), sizeof(settings.apiBaseUrl));
  writeCString(settings.deviceId, webServer.arg("deviceId"), sizeof(settings.deviceId));
  writeCString(settings.linkCode, webServer.arg("linkCode"), sizeof(settings.linkCode));
  saveSettings();

  webServer.send(200, "text/html; charset=utf-8", "<html><body><h1>保存成功，设备即将重启</h1></body></html>");
  delay(1000);
  ESP.restart();
}

bool ensureProvisioned() {
  if (strlen(settings.deviceToken) > 0) {
    return true;
  }

  if (strlen(settings.linkCode) == 0) {
    Serial.println("missing link code");
    return false;
  }

  return pairDevice();
}

bool pairDevice() {
  String url = String(settings.apiBaseUrl) + "/api/public/device/pair";

#ifdef ESP32
  HTTPClient http;
  http.begin(url);
#else
  HTTPClient http;
  WiFiClient pairClient;
  http.begin(pairClient, url);
#endif

  http.addHeader("Content-Type", "application/json");
  DynamicJsonDocument requestDoc(512);
  requestDoc["deviceId"] = settings.deviceId;
  requestDoc["linkCode"] = settings.linkCode;
#ifdef ESP32
  requestDoc["platform"] = "esp32";
#else
  requestDoc["platform"] = "esp01s";
#endif

  String requestBody;
  serializeJson(requestDoc, requestBody);
  int httpCode = http.POST(requestBody);
  if (httpCode <= 0) {
    http.end();
    return false;
  }

  DynamicJsonDocument responseDoc(12288);
  DeserializationError error = deserializeJson(responseDoc, http.getString());
  http.end();
  if (error) {
    return false;
  }

  String token = responseDoc["deviceToken"] | "";
  if (token.length() == 0) {
    return false;
  }

  writeCString(settings.deviceToken, token, sizeof(settings.deviceToken));
  saveSettings();
  return true;
}

void bootstrapFromServer() {
  String url = String(settings.apiBaseUrl) + "/api/public/device/bootstrap?deviceId=" + settings.deviceId + "&deviceToken=" + settings.deviceToken;

#ifdef ESP32
  HTTPClient http;
  http.begin(url);
#else
  HTTPClient http;
  WiFiClient bootstrapClient;
  http.begin(bootstrapClient, url);
#endif

  int httpCode = http.GET();
  if (httpCode <= 0) {
    http.end();
    return;
  }

  DynamicJsonDocument doc(12288);
  DeserializationError error = deserializeJson(doc, http.getString());
  http.end();
  if (error) {
    return;
  }

  downlinkTopic = doc["downlinkTopic"].as<String>();
  upstreamTopic = doc["upstreamTopic"].as<String>();
  loadTimersFromBootstrap(doc["device"].as<JsonObject>());

  channelCount = 0;
  JsonArray driverInstances = doc["driverInstances"].as<JsonArray>();
  for (JsonVariant instance : driverInstances) {
    if (channelCount >= 8) {
      break;
    }
    appendChannelFromDriverInstance(instance);
  }

  if (channelCount > 0) {
    return;
  }

  JsonArray capabilities = doc["device"]["capabilities"].as<JsonArray>();
  for (JsonVariant capability : capabilities) {
    if (channelCount >= 8) {
      break;
    }
    appendChannelFromCapability(capability);
  }
}

void setupChannelPins() {
  for (size_t i = 0; i < channelCount; i++) {
    if (channels[i].gpio < 0) {
      continue;
    }
    pinMode(channels[i].gpio, OUTPUT);
    if (channels[i].kind == CHANNEL_RELAY) {
      bool defaultOn = channels[i].defaultState == "on";
      digitalWrite(channels[i].gpio, relayLevel(channels[i], defaultOn));
      channels[i].currentState = defaultOn ? "on" : "off";
      channels[i].currentMode = "switch";
    } else {
#ifdef ESP32
      ledcSetup(channels[i].pwm.channel, channels[i].pwm.frequency, 10);
      ledcAttachPin(channels[i].gpio, channels[i].pwm.channel);
      ledcWrite(channels[i].pwm.channel, 0);
#else
      analogWriteRange(channels[i].pwm.maxDuty);
      analogWriteFreq(channels[i].pwm.frequency);
      analogWrite(channels[i].gpio, 0);
#endif
      channels[i].currentDuty = 0;
      channels[i].currentMode = "direct";
    }
  }
}

void ensureMqttConnected() {
  while (!mqttClient.connected()) {
    String clientId = String("seacontroll-") + settings.deviceId;
    bool connected = mqttClient.connect(clientId.c_str());
    if (connected) {
      mqttClient.subscribe(downlinkTopic.c_str());
      publishStateReport();
    } else {
      delay(1000);
    }
  }
}

void onMessage(char* topic, byte* payload, unsigned int length) {
  DynamicJsonDocument doc(8192);
  DeserializationError error = deserializeJson(doc, payload, length);
  if (error) {
    return;
  }

  JsonObject command = doc["command"];
  String kind = command["kind"] | "";
  String operation = command["operation"] | "";
  JsonObject params = command["params"];

  if (kind == "system" && operation == "reportState") {
    publishStateReport();
    return;
  }

  if (kind == "relay" && operation == "switch") {
    applyRelay(command["targetId"] | "", params["state"] | "off");
  } else if (kind == "relay" && operation == "toggle") {
    applyRelayToggle(command["targetId"] | "");
  } else if (kind == "mos_pwm" && operation == "direct") {
    applyDirectPWM(command["targetId"] | "", params["duty"] | 0);
  } else if (kind == "mos_pwm" && operation == "linearRamp") {
    applyLinearRamp(command["targetId"] | "", params["from"] | 0, params["to"] | 0, params["durationMs"] | 1000);
  } else if (kind == "mos_pwm" && operation == "sequence") {
    applySequence(command["targetId"] | "", params["steps"].as<JsonArray>(), params["loop"] | false);
  } else if (kind == "mos_pwm" && operation == "stop") {
    applyDirectPWM(command["targetId"] | "", 0);
    ChannelConfig* channel = findChannel(command["targetId"] | "");
    if (channel != nullptr) {
      channel->currentMode = "stop";
    }
  } else if (kind == "mos_pwm" && operation == "softStart") {
    applySoftStart(command["targetId"] | "", params["to"] | 0, params["durationMs"] | 1000);
  } else if (kind == "mos_pwm" && operation == "softStop") {
    applySoftStop(command["targetId"] | "", params["from"] | -1, params["durationMs"] | 1000);
  } else if (kind == "mos_pwm" && operation == "pulse") {
    applyPulse(command["targetId"] | "", params["duty"] | 0, params["durationMs"] | 1000, params["intervalMs"] | 1000, params["repeat"] | 1, params["loop"] | false);
  } else if (kind == "virtual_group" && operation == "sequenceGroup") {
    applySequenceGroup(params["channels"].as<JsonArray>(), params["loop"] | false);
  }

  publishStateReport();
}

ChannelConfig* findChannel(const String& targetId) {
  for (size_t i = 0; i < channelCount; i++) {
    if (channels[i].id == targetId) {
      return &channels[i];
    }
  }
  return nullptr;
}

void applyRelay(String targetId, String state) {
  ChannelConfig* channel = findChannel(targetId);
  if (channel == nullptr) {
    return;
  }
  digitalWrite(channel->gpio, relayLevel(*channel, state == "on"));
  channel->currentState = state;
  channel->currentMode = "switch";
}

void applyRelayToggle(String targetId) {
  ChannelConfig* channel = findChannel(targetId);
  if (channel == nullptr) {
    return;
  }
  if (channel->currentState == "on") {
    applyRelay(targetId, "off");
  } else {
    applyRelay(targetId, "on");
  }
  channel->currentMode = "toggle";
}

void applyDirectPWM(String targetId, int duty) {
  ChannelConfig* channel = findChannel(targetId);
  if (channel == nullptr) {
    return;
  }
  writePWM(*channel, duty);
  channel->currentMode = "direct";
}

void applyLinearRamp(String targetId, int fromDuty, int toDuty, int durationMs) {
  ChannelConfig* channel = findChannel(targetId);
  if (channel == nullptr) {
    return;
  }
  int steps = 40;
  channel->currentMode = "linearRamp";
  for (int i = 0; i <= steps; i++) {
    int duty = fromDuty + ((toDuty - fromDuty) * i / steps);
    writePWM(*channel, duty);
    delay(durationMs / steps);
  }
}

void applySoftStart(String targetId, int toDuty, int durationMs) {
  applyLinearRamp(targetId, 0, toDuty, durationMs);
  ChannelConfig* channel = findChannel(targetId);
  if (channel != nullptr) {
    channel->currentMode = "softStart";
  }
}

void applySoftStop(String targetId, int fromDuty, int durationMs) {
  ChannelConfig* channel = findChannel(targetId);
  if (channel == nullptr) {
    return;
  }
  int startDuty = fromDuty;
  if (startDuty < 0) {
    startDuty = channel->currentDuty;
  }
  applyLinearRamp(targetId, startDuty, 0, durationMs);
  channel->currentMode = "softStop";
}

void applySequence(String targetId, JsonArray steps, bool loop) {
  ChannelConfig* channel = findChannel(targetId);
  if (channel == nullptr) {
    return;
  }
  channel->currentMode = "sequence";
  runSteps(*channel, steps, loop);
}

void applySequenceGroup(JsonArray channelsArray, bool loop) {
  do {
    for (JsonVariant channelVariant : channelsArray) {
      String targetId = channelVariant["targetId"] | "";
      ChannelConfig* channel = findChannel(targetId);
      if (channel == nullptr) {
        continue;
      }
      JsonArray steps = channelVariant["steps"].as<JsonArray>();
      runSteps(*channel, steps, false);
    }
  } while (loop);
}

void applyPulse(String targetId, int duty, int holdMs, int intervalMs, int repeat, bool loop) {
  ChannelConfig* channel = findChannel(targetId);
  if (channel == nullptr) {
    return;
  }
  if (repeat <= 0) {
    repeat = 1;
  }
  channel->currentMode = "pulse";
  do {
    for (int index = 0; index < repeat; index++) {
      writePWM(*channel, duty);
      delay(holdMs);
      writePWM(*channel, 0);
      delay(intervalMs);
    }
  } while (loop);
}

void runSteps(ChannelConfig& channel, JsonArray steps, bool loop) {
  do {
    for (JsonVariant step : steps) {
      int duty = step["duty"] | 0;
      int durationMs = step["durationMs"] | 1000;
      writePWM(channel, duty);
      delay(durationMs);
    }
  } while (loop);
}

void writePWM(ChannelConfig& channel, int duty) {
  duty = constrain(duty, channel.pwm.minDuty, channel.pwm.maxDuty);
  channel.currentDuty = duty;
#ifdef ESP32
  ledcWrite(channel.pwm.channel, duty);
#else
  analogWrite(channel.gpio, duty);
#endif
}

int relayLevel(ChannelConfig& channel, bool on) {
  bool activeLow = channel.activeLevel == "low";
  if (on) {
    return activeLow ? LOW : HIGH;
  }
  return activeLow ? HIGH : LOW;
}

void publishStateReport() {
  if (!mqttClient.connected()) {
    return;
  }

  DynamicJsonDocument doc(4096);
  doc["deviceId"] = settings.deviceId;
  doc["online"] = true;
  doc["ip"] = WiFi.localIP().toString();
  doc["rssi"] = WiFi.RSSI();
  doc["uptimeMs"] = millis();

  JsonObject channelsObject = doc.createNestedObject("channels");
  for (size_t i = 0; i < channelCount; i++) {
    JsonObject channelObject = channelsObject.createNestedObject(channels[i].id);
    channelObject["targetId"] = channels[i].id;
    channelObject["updatedAt"] = millis();
    channelObject["status"] = "ok";
    if (channels[i].kind == CHANNEL_RELAY) {
      channelObject["kind"] = "relay";
      channelObject["state"] = channels[i].currentState;
    } else {
      channelObject["kind"] = "mos_pwm";
      channelObject["duty"] = channels[i].currentDuty;
      channelObject["mode"] = channels[i].currentMode;
    }
  }

  String payload;
  serializeJson(doc, payload);
  mqttClient.publish(upstreamTopic.c_str(), payload.c_str());

  String url = String(settings.apiBaseUrl) + "/api/public/device-state/report?deviceId=" + settings.deviceId + "&deviceToken=" + settings.deviceToken;
#ifdef ESP32
  HTTPClient http;
  http.begin(url);
#else
  HTTPClient http;
  WiFiClient reportClient;
  http.begin(reportClient, url);
#endif
  http.addHeader("Content-Type", "application/json");
  http.POST(payload);
  http.end();
}

void writeCString(char* target, const String& source, size_t size) {
  memset(target, 0, size);
  source.substring(0, size - 1).toCharArray(target, size);
}

String extractHost(const String& url) {
  String value = url;
  int schemeIndex = value.indexOf("://");
  if (schemeIndex >= 0) {
    value = value.substring(schemeIndex + 3);
  }
  int slashIndex = value.indexOf("/");
  if (slashIndex >= 0) {
    value = value.substring(0, slashIndex);
  }
  int colonIndex = value.indexOf(":");
  if (colonIndex >= 0) {
    value = value.substring(0, colonIndex);
  }
  mqttPort = DEFAULT_MQTT_PORT;
  return value;
}
