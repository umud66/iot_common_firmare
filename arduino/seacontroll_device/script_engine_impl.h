#ifdef ESP8266
#define SCRIPT_MAX_LINES 40
#define SCRIPT_MAX_STATEMENTS 56
#define SCRIPT_MAX_STACK 8
#define SCRIPT_MAX_VALUES 10
#define SCRIPT_MAX_TOKENS 48
#define SCRIPT_MAX_ARGS 6
#else
#define SCRIPT_MAX_LINES 96
#define SCRIPT_MAX_STATEMENTS 128
#define SCRIPT_MAX_STACK 16
#define SCRIPT_MAX_VALUES 24
#define SCRIPT_MAX_TOKENS 96
#define SCRIPT_MAX_ARGS 8
#endif

enum ScriptStmtKind {
  SCRIPT_STMT_CONST = 1,
  SCRIPT_STMT_VAR = 2,
  SCRIPT_STMT_ASSIGN = 3,
  SCRIPT_STMT_IF = 4,
  SCRIPT_STMT_CALL = 5
};

enum ScriptValueType {
  SCRIPT_VALUE_NULL = 0,
  SCRIPT_VALUE_NUMBER = 1,
  SCRIPT_VALUE_BOOL = 2,
  SCRIPT_VALUE_STRING = 3
};

enum ScriptTokenKind {
  SCRIPT_TOKEN_IDENT = 1,
  SCRIPT_TOKEN_NUMBER = 2,
  SCRIPT_TOKEN_STRING = 3,
  SCRIPT_TOKEN_OP = 4,
  SCRIPT_TOKEN_PUNCT = 5
};

struct ScriptValue {
  uint8_t type;
  float number;
  bool booleanValue;
  String stringValue;
};

struct ScriptNamedValue {
  String name;
  ScriptValue value;
  bool used;
};

struct ScriptSourceLine {
  int indent;
  String text;
  int line;
};

struct ScriptStmt {
  uint8_t kind;
  String name;
  String expr;
  uint16_t nextIndex;
  uint16_t bodyFirst;
  uint16_t elseFirst;
  uint16_t line;
};

struct ScriptBlock {
  uint16_t first;
};

struct ScriptProgram {
  bool valid;
  ScriptStmt stmts[SCRIPT_MAX_STATEMENTS];
  uint16_t stmtCount;
  ScriptBlock initBlock;
  ScriptBlock loopBlock;
};

struct ScriptFrame {
  uint16_t stmtIndex;
};

struct ScriptToken {
  uint8_t kind;
  String value;
};

struct ScriptEvalContext {
  ScriptToken tokens[SCRIPT_MAX_TOKENS];
  int tokenCount;
  int pos;
  bool statementOnly;
  bool pauseRequested;
  unsigned long nowMs;
  String error;
};

struct ScriptRuntimeState {
  bool loaded;
  String targetId;
  ScriptProgram program;
  ScriptNamedValue consts[SCRIPT_MAX_VALUES];
  ScriptNamedValue vars[SCRIPT_MAX_VALUES];
  ScriptFrame resume[SCRIPT_MAX_STACK];
  uint8_t resumeCount;
  unsigned long wakeAtMs;
  unsigned long lastRunAtMs;
  uint32_t randState;
  String lastError;
};

ScriptRuntimeState scriptRuntimes[8];

ScriptValue makeNullValue() {
  ScriptValue value;
  value.type = SCRIPT_VALUE_NULL;
  value.number = 0.0f;
  value.booleanValue = false;
  value.stringValue = "";
  return value;
}

ScriptValue makeNumberValue(float item) {
  ScriptValue value = makeNullValue();
  value.type = SCRIPT_VALUE_NUMBER;
  value.number = item;
  return value;
}

ScriptValue makeBoolValue(bool item) {
  ScriptValue value = makeNullValue();
  value.type = SCRIPT_VALUE_BOOL;
  value.booleanValue = item;
  return value;
}

ScriptValue makeStringValue(const String& item) {
  ScriptValue value = makeNullValue();
  value.type = SCRIPT_VALUE_STRING;
  value.stringValue = item;
  return value;
}

float scriptAsFloat(const ScriptValue& value) {
  if (value.type == SCRIPT_VALUE_NUMBER) {
    return value.number;
  }
  if (value.type == SCRIPT_VALUE_BOOL) {
    return value.booleanValue ? 1.0f : 0.0f;
  }
  if (value.type == SCRIPT_VALUE_STRING) {
    return value.stringValue.toFloat();
  }
  return 0.0f;
}

bool scriptAsBool(const ScriptValue& value) {
  if (value.type == SCRIPT_VALUE_BOOL) {
    return value.booleanValue;
  }
  if (value.type == SCRIPT_VALUE_NUMBER) {
    return fabsf(value.number) > 0.00001f;
  }
  if (value.type == SCRIPT_VALUE_STRING) {
    String text = value.stringValue;
    text.toLowerCase();
    return text == "true" || text == "on" || text == "1";
  }
  return false;
}

String scriptAsString(const ScriptValue& value) {
  if (value.type == SCRIPT_VALUE_STRING) {
    return value.stringValue;
  }
  if (value.type == SCRIPT_VALUE_BOOL) {
    return value.booleanValue ? "true" : "false";
  }
  if (value.type == SCRIPT_VALUE_NUMBER) {
    return String(value.number, 4);
  }
  return "";
}

bool scriptCompareEq(const ScriptValue& left, const ScriptValue& right) {
  if (left.type == SCRIPT_VALUE_STRING || right.type == SCRIPT_VALUE_STRING) {
    return scriptAsString(left) == scriptAsString(right);
  }
  if (left.type == SCRIPT_VALUE_BOOL || right.type == SCRIPT_VALUE_BOOL) {
    return scriptAsBool(left) == scriptAsBool(right);
  }
  return fabsf(scriptAsFloat(left) - scriptAsFloat(right)) < 0.00001f;
}

int scriptChannelIndex(const String& targetId) {
  return channelIndex(targetId);
}

ScriptRuntimeState* scriptRuntimeForTarget(const String& targetId) {
  int index = scriptChannelIndex(targetId);
  if (index < 0 || index >= 8) {
    return nullptr;
  }
  return &scriptRuntimes[index];
}

void clearScriptNamedValues(ScriptNamedValue values[], int capacity) {
  for (int i = 0; i < capacity; i++) {
    values[i].used = false;
    values[i].name = "";
    values[i].value = makeNullValue();
  }
}

void resetScriptRuntimeState(ScriptRuntimeState& runtime) {
  runtime.loaded = false;
  runtime.targetId = "";
  runtime.program.valid = false;
  runtime.program.stmtCount = 0;
  runtime.program.initBlock.first = 0xFFFF;
  runtime.program.loopBlock.first = 0xFFFF;
  clearScriptNamedValues(runtime.consts, SCRIPT_MAX_VALUES);
  clearScriptNamedValues(runtime.vars, SCRIPT_MAX_VALUES);
  runtime.resumeCount = 0;
  runtime.wakeAtMs = 0;
  runtime.lastRunAtMs = 0;
  runtime.randState = (uint32_t)(micros() ^ millis() ^ 0x9e3779b9UL);
  runtime.lastError = "";
}

ScriptNamedValue* findScriptNamedValue(ScriptNamedValue values[], int capacity, const String& name) {
  for (int i = 0; i < capacity; i++) {
    if (values[i].used && values[i].name == name) {
      return &values[i];
    }
  }
  return nullptr;
}

bool setScriptNamedValue(ScriptNamedValue values[], int capacity, const String& name, const ScriptValue& value) {
  ScriptNamedValue* existing = findScriptNamedValue(values, capacity, name);
  if (existing != nullptr) {
    existing->value = value;
    return true;
  }
  for (int i = 0; i < capacity; i++) {
    if (!values[i].used) {
      values[i].used = true;
      values[i].name = name;
      values[i].value = value;
      return true;
    }
  }
  return false;
}

bool resolveScriptValue(ScriptRuntimeState& runtime, const String& name, ScriptValue& outValue) {
  ScriptNamedValue* variable = findScriptNamedValue(runtime.vars, SCRIPT_MAX_VALUES, name);
  if (variable != nullptr) {
    outValue = variable->value;
    return true;
  }
  ScriptNamedValue* constant = findScriptNamedValue(runtime.consts, SCRIPT_MAX_VALUES, name);
  if (constant != nullptr) {
    outValue = constant->value;
    return true;
  }
  return false;
}

int countLeadingSpaces(const String& text) {
  int count = 0;
  while (count < text.length() && text.charAt(count) == ' ') {
    count += 1;
  }
  return count;
}

int findTopLevelAssignment(const String& text) {
  bool inQuotes = false;
  char quoteChar = 0;
  int depth = 0;
  for (int i = 0; i < text.length(); i++) {
    char current = text.charAt(i);
    if ((current == '"' || current == '\'') && (i == 0 || text.charAt(i - 1) != '\\')) {
      if (!inQuotes) {
        inQuotes = true;
        quoteChar = current;
      } else if (quoteChar == current) {
        inQuotes = false;
      }
      continue;
    }
    if (inQuotes) {
      continue;
    }
    if (current == '(' || current == '[') {
      depth += 1;
      continue;
    }
    if (current == ')' || current == ']') {
      depth -= 1;
      continue;
    }
    if (depth != 0 || current != '=') {
      continue;
    }
    char prev = i > 0 ? text.charAt(i - 1) : 0;
    char next = i + 1 < text.length() ? text.charAt(i + 1) : 0;
    if (prev == '!' || prev == '<' || prev == '>' || next == '=') {
      continue;
    }
    return i;
  }
  return -1;
}

bool addScriptStatement(ScriptProgram& program, const ScriptStmt& stmt, uint16_t& indexOut) {
  if (program.stmtCount >= SCRIPT_MAX_STATEMENTS) {
    return false;
  }
  indexOut = program.stmtCount;
  program.stmts[program.stmtCount++] = stmt;
  return true;
}

bool parseScriptStatementText(const ScriptSourceLine& line, ScriptStmt& stmt, String& error) {
  String text = trimCopy(line.text);
  stmt.kind = 0;
  stmt.name = "";
  stmt.expr = "";
  stmt.nextIndex = 0xFFFF;
  stmt.bodyFirst = 0xFFFF;
  stmt.elseFirst = 0xFFFF;
  stmt.line = line.line;

  if (text.startsWith("const ")) {
    int assignIndex = findTopLevelAssignment(text);
    if (assignIndex <= 6) {
      error = "const 语法非法";
      return false;
    }
    stmt.kind = SCRIPT_STMT_CONST;
    stmt.name = trimCopy(text.substring(6, assignIndex));
    stmt.expr = trimCopy(text.substring(assignIndex + 1));
    return stmt.name.length() > 0 && stmt.expr.length() > 0;
  }
  if (text.startsWith("var ")) {
    int assignIndex = findTopLevelAssignment(text);
    if (assignIndex <= 4) {
      error = "var 语法非法";
      return false;
    }
    stmt.kind = SCRIPT_STMT_VAR;
    stmt.name = trimCopy(text.substring(4, assignIndex));
    stmt.expr = trimCopy(text.substring(assignIndex + 1));
    return stmt.name.length() > 0 && stmt.expr.length() > 0;
  }
  if (text.startsWith("if ") && text.endsWith(":")) {
    stmt.kind = SCRIPT_STMT_IF;
    stmt.expr = trimCopy(text.substring(3, text.length() - 1));
    return stmt.expr.length() > 0;
  }
  int assignIndex = findTopLevelAssignment(text);
  if (assignIndex > 0) {
    stmt.kind = SCRIPT_STMT_ASSIGN;
    stmt.name = trimCopy(text.substring(0, assignIndex));
    stmt.expr = trimCopy(text.substring(assignIndex + 1));
    return stmt.name.length() > 0 && stmt.expr.length() > 0;
  }
  stmt.kind = SCRIPT_STMT_CALL;
  stmt.expr = text;
  return stmt.expr.length() > 0;
}

bool parseScriptBlock(ScriptSourceLine lines[], int lineCount, int& cursor, int indent, ScriptProgram& program, ScriptBlock& block, String& error);

bool parseUserScriptProgram(const String& source, ScriptProgram& program, String& error) {
  program.valid = false;
  program.stmtCount = 0;
  program.initBlock.first = 0xFFFF;
  program.loopBlock.first = 0xFFFF;

  ScriptSourceLine lines[SCRIPT_MAX_LINES];
  int lineCount = 0;
  int lineNo = 1;
  int start = 0;
  for (int i = 0; i <= source.length(); i++) {
    if (i == source.length() || source.charAt(i) == '\n') {
      String raw = source.substring(start, i);
      raw.replace("\r", "");
      String trimmed = trimCopy(raw);
      if (trimmed.length() > 0) {
        if (lineCount >= SCRIPT_MAX_LINES) {
          error = "脚本行数超出限制";
          return false;
        }
        lines[lineCount].indent = countLeadingSpaces(raw);
        lines[lineCount].text = trimmed;
        lines[lineCount].line = lineNo;
        lineCount += 1;
      }
      start = i + 1;
      lineNo += 1;
    }
  }

  int cursor = 0;
  uint16_t previousTopLevel = 0xFFFF;
  while (cursor < lineCount) {
    ScriptSourceLine line = lines[cursor];
    if (line.indent != 0) {
      error = "顶层缩进非法";
      return false;
    }
    if (line.text == "loop:") {
      cursor += 1;
      if (!parseScriptBlock(lines, lineCount, cursor, 2, program, program.loopBlock, error)) {
        return false;
      }
      break;
    }
    ScriptStmt stmt;
    if (!parseScriptStatementText(line, stmt, error)) {
      error = "line " + String(line.line) + ": " + error;
      return false;
    }
    uint16_t stmtIndex = 0;
    if (!addScriptStatement(program, stmt, stmtIndex)) {
      error = "脚本语句超出限制";
      return false;
    }
    if (program.initBlock.first == 0xFFFF) {
      program.initBlock.first = stmtIndex;
    }
    if (previousTopLevel != 0xFFFF) {
      program.stmts[previousTopLevel].nextIndex = stmtIndex;
    }
    previousTopLevel = stmtIndex;
    cursor += 1;
    if (stmt.kind == SCRIPT_STMT_IF) {
      ScriptBlock body;
      if (!parseScriptBlock(lines, lineCount, cursor, line.indent + 2, program, body, error)) {
        return false;
      }
      program.stmts[stmtIndex].bodyFirst = body.first;
      if (cursor < lineCount && lines[cursor].indent == line.indent && lines[cursor].text == "else:") {
        cursor += 1;
        ScriptBlock elseBlock;
        if (!parseScriptBlock(lines, lineCount, cursor, line.indent + 2, program, elseBlock, error)) {
          return false;
        }
        program.stmts[stmtIndex].elseFirst = elseBlock.first;
      }
    }
  }
  program.valid = true;
  return true;
}

bool parseScriptBlock(ScriptSourceLine lines[], int lineCount, int& cursor, int indent, ScriptProgram& program, ScriptBlock& block, String& error) {
  block.first = 0xFFFF;
  uint16_t previousIndex = 0xFFFF;
  while (cursor < lineCount) {
    ScriptSourceLine line = lines[cursor];
    if (line.indent < indent) {
      break;
    }
    if (line.indent > indent) {
      error = "line " + String(line.line) + ": 缩进非法";
      return false;
    }
    if (line.text == "else:") {
      break;
    }
    ScriptStmt stmt;
    if (!parseScriptStatementText(line, stmt, error)) {
      error = "line " + String(line.line) + ": " + error;
      return false;
    }
    uint16_t stmtIndex = 0;
    if (!addScriptStatement(program, stmt, stmtIndex)) {
      error = "脚本语句超出限制";
      return false;
    }
    if (block.first == 0xFFFF) {
      block.first = stmtIndex;
    }
    if (previousIndex != 0xFFFF) {
      program.stmts[previousIndex].nextIndex = stmtIndex;
    }
    previousIndex = stmtIndex;
    cursor += 1;
    if (stmt.kind == SCRIPT_STMT_IF) {
      ScriptBlock body;
      if (!parseScriptBlock(lines, lineCount, cursor, indent + 2, program, body, error)) {
        return false;
      }
      program.stmts[stmtIndex].bodyFirst = body.first;
      if (cursor < lineCount && lines[cursor].indent == indent && lines[cursor].text == "else:") {
        cursor += 1;
        ScriptBlock elseBlock;
        if (!parseScriptBlock(lines, lineCount, cursor, indent + 2, program, elseBlock, error)) {
          return false;
        }
        program.stmts[stmtIndex].elseFirst = elseBlock.first;
      }
    }
  }
  return true;
}

bool addScriptToken(ScriptEvalContext& ctx, uint8_t kind, const String& value) {
  if (ctx.tokenCount >= SCRIPT_MAX_TOKENS) {
    ctx.error = "表达式过长";
    return false;
  }
  ctx.tokens[ctx.tokenCount].kind = kind;
  ctx.tokens[ctx.tokenCount].value = value;
  ctx.tokenCount += 1;
  return true;
}

bool tokenizeScriptExpression(const String& input, ScriptEvalContext& ctx) {
  ctx.tokenCount = 0;
  ctx.pos = 0;
  for (int i = 0; i < input.length();) {
    char current = input.charAt(i);
    if (current == ' ' || current == '\t') {
      i += 1;
      continue;
    }
    if ((current >= '0' && current <= '9') || (current == '.' && i + 1 < input.length() && isDigit(input.charAt(i + 1)))) {
      int start = i;
      i += 1;
      while (i < input.length() && (isDigit(input.charAt(i)) || input.charAt(i) == '.')) {
        i += 1;
      }
      if (!addScriptToken(ctx, SCRIPT_TOKEN_NUMBER, input.substring(start, i))) {
        return false;
      }
      continue;
    }
    if (current == '"' || current == '\'') {
      char quote = current;
      i += 1;
      String value = "";
      while (i < input.length() && input.charAt(i) != quote) {
        value += input.charAt(i);
        i += 1;
      }
      if (i >= input.length()) {
        ctx.error = "字符串未闭合";
        return false;
      }
      i += 1;
      if (!addScriptToken(ctx, SCRIPT_TOKEN_STRING, value)) {
        return false;
      }
      continue;
    }
    if (isAlpha(current) || current == '_') {
      int start = i;
      i += 1;
      while (i < input.length() && (isAlphaNumeric(input.charAt(i)) || input.charAt(i) == '_' || input.charAt(i) == '.')) {
        i += 1;
      }
      if (!addScriptToken(ctx, SCRIPT_TOKEN_IDENT, input.substring(start, i))) {
        return false;
      }
      continue;
    }
    String pair = i + 1 < input.length() ? input.substring(i, i + 2) : "";
    if (pair == ">=" || pair == "<=" || pair == "==" || pair == "!=") {
      if (!addScriptToken(ctx, SCRIPT_TOKEN_OP, pair)) {
        return false;
      }
      i += 2;
      continue;
    }
    if (current == '+' || current == '-' || current == '*' || current == '/' || current == '>' || current == '<') {
      if (!addScriptToken(ctx, SCRIPT_TOKEN_OP, String(current))) {
        return false;
      }
      i += 1;
      continue;
    }
    if (current == '(' || current == ')' || current == ',') {
      if (!addScriptToken(ctx, SCRIPT_TOKEN_PUNCT, String(current))) {
        return false;
      }
      i += 1;
      continue;
    }
    ctx.error = "不支持的字符: " + String(current);
    return false;
  }
  return true;
}

bool scriptMatchToken(ScriptEvalContext& ctx, uint8_t kind, const String& value) {
  if (ctx.pos >= ctx.tokenCount) {
    return false;
  }
  if (ctx.tokens[ctx.pos].kind != kind) {
    return false;
  }
  if (value.length() > 0 && ctx.tokens[ctx.pos].value != value) {
    return false;
  }
  ctx.pos += 1;
  return true;
}

bool scriptCheckToken(ScriptEvalContext& ctx, uint8_t kind, const String& value) {
  if (ctx.pos >= ctx.tokenCount) {
    return false;
  }
  if (ctx.tokens[ctx.pos].kind != kind) {
    return false;
  }
  return value.length() == 0 || ctx.tokens[ctx.pos].value == value;
}

bool scriptParseExpression(ScriptRuntimeState& runtime, ScriptEvalContext& ctx, ScriptValue& outValue);

float scriptMapRange(float value, float inMin, float inMax, float outMin, float outMax) {
  if (fabsf(inMax - inMin) < 0.00001f) {
    return outMin;
  }
  return outMin + ((value - inMin) / (inMax - inMin)) * (outMax - outMin);
}

float scriptRandomRange(ScriptRuntimeState& runtime, float minValue, float maxValue) {
  if (maxValue < minValue) {
    float temp = minValue;
    minValue = maxValue;
    maxValue = temp;
  }
  if (fabsf(maxValue - minValue) < 0.00001f) {
    return minValue;
  }
  if (runtime.randState == 0) {
    runtime.randState = (uint32_t)(micros() ^ millis() ^ 0x9e3779b9UL);
  }
  runtime.randState = runtime.randState * 1664525UL + 1013904223UL;
  float ratio = (float)(runtime.randState & 0x00FFFFFFUL) / (float)0x01000000UL;
  return minValue + ratio * (maxValue - minValue);
}

void writeScriptMetric(const String& name, float value) {
  for (int i = 0; i < 16; i++) {
    if (metricEntries[i].used && metricEntries[i].key == name) {
      metricEntries[i].value = value;
      return;
    }
  }
  for (int i = 0; i < 16; i++) {
    if (!metricEntries[i].used) {
      metricEntries[i].used = true;
      metricEntries[i].key = name;
      metricEntries[i].value = value;
      return;
    }
  }
}

bool scriptReadValue(const String& key, ScriptValue& outValue, unsigned long nowMs) {
  int split = key.indexOf('.');
  if (split <= 0) {
    return false;
  }
  String target = key.substring(0, split);
  String field = key.substring(split + 1);
  if (target == "system") {
    if (field == "uptimeSec") {
      outValue = makeNumberValue((float)millis() / 1000.0f);
      return true;
    }
    if (field == "tickSec") {
      outValue = makeNumberValue((float)nowMs / 1000.0f);
      return true;
    }
    return false;
  }
  ChannelConfig* channel = findChannel(target);
  if (channel == nullptr) {
    return false;
  }
  if (field == "duty") {
    outValue = makeNumberValue((float)channel->currentDuty);
    return true;
  }
  if (field == "state") {
    outValue = makeStringValue(channel->currentState);
    return true;
  }
  if (field == "mode") {
    outValue = makeStringValue(channel->currentMode);
    return true;
  }
  if (field == "status") {
    outValue = makeStringValue(channel->currentStatus);
    return true;
  }
  return false;
}

bool scriptCallFunction(ScriptRuntimeState& runtime, ScriptEvalContext& ctx, const String& name, ScriptValue args[], int argCount, ScriptValue& outValue) {
  if (name == "min" && argCount > 0) {
    float result = scriptAsFloat(args[0]);
    for (int i = 1; i < argCount; i++) {
      result = min(result, scriptAsFloat(args[i]));
    }
    outValue = makeNumberValue(result);
    return true;
  }
  if (name == "max" && argCount > 0) {
    float result = scriptAsFloat(args[0]);
    for (int i = 1; i < argCount; i++) {
      result = max(result, scriptAsFloat(args[i]));
    }
    outValue = makeNumberValue(result);
    return true;
  }
  if (name == "clamp" && argCount >= 3) {
    outValue = makeNumberValue(clampFloat(scriptAsFloat(args[0]), scriptAsFloat(args[1]), scriptAsFloat(args[2])));
    return true;
  }
  if (name == "abs" && argCount >= 1) {
    outValue = makeNumberValue(fabsf(scriptAsFloat(args[0])));
    return true;
  }
  if (name == "floor" && argCount >= 1) {
    outValue = makeNumberValue(floorf(scriptAsFloat(args[0])));
    return true;
  }
  if (name == "ceil" && argCount >= 1) {
    outValue = makeNumberValue(ceilf(scriptAsFloat(args[0])));
    return true;
  }
  if (name == "sin" && argCount >= 1) {
    outValue = makeNumberValue(sinf(scriptAsFloat(args[0])));
    return true;
  }
  if (name == "cos" && argCount >= 1) {
    outValue = makeNumberValue(cosf(scriptAsFloat(args[0])));
    return true;
  }
  if (name == "pow" && argCount >= 2) {
    outValue = makeNumberValue(powf(scriptAsFloat(args[0]), scriptAsFloat(args[1])));
    return true;
  }
  if (name == "norm" && argCount >= 3) {
    float minValue = scriptAsFloat(args[1]);
    float maxValue = scriptAsFloat(args[2]);
    outValue = makeNumberValue(fabsf(maxValue - minValue) < 0.00001f ? 0.0f : clampFloat((scriptAsFloat(args[0]) - minValue) / (maxValue - minValue), 0.0f, 1.0f));
    return true;
  }
  if (name == "map" && argCount >= 5) {
    outValue = makeNumberValue(scriptMapRange(scriptAsFloat(args[0]), scriptAsFloat(args[1]), scriptAsFloat(args[2]), scriptAsFloat(args[3]), scriptAsFloat(args[4])));
    return true;
  }
  if (name == "wave" && argCount >= 5) {
    outValue = makeNumberValue(clampFloat(scriptMapRange(scriptAsFloat(args[0]), scriptAsFloat(args[1]), scriptAsFloat(args[2]), scriptAsFloat(args[3]), scriptAsFloat(args[4])), scriptAsFloat(args[3]), scriptAsFloat(args[4])));
    return true;
  }
  if (name == "wrap_add" && argCount >= 3) {
    float value = scriptAsFloat(args[0]) + scriptAsFloat(args[1]);
    float limit = scriptAsFloat(args[2]);
    if (fabsf(limit) < 0.00001f) {
      outValue = makeNumberValue(0.0f);
    } else {
      while (value > limit) {
        value -= limit;
      }
      outValue = makeNumberValue(value);
    }
    return true;
  }
  if (name == "bezier" && argCount >= 5) {
    outValue = makeNumberValue(cubicBezier(scriptAsFloat(args[0]), scriptAsFloat(args[1]), scriptAsFloat(args[2]), scriptAsFloat(args[3]), scriptAsFloat(args[4])));
    return true;
  }
  if (name == "rand" && argCount >= 2) {
    outValue = makeNumberValue(scriptRandomRange(runtime, scriptAsFloat(args[0]), scriptAsFloat(args[1])));
    return true;
  }
  if (name == "read" && argCount >= 1) {
    if (!scriptReadValue(scriptAsString(args[0]), outValue, ctx.nowMs)) {
      ctx.error = "read 目标不存在: " + scriptAsString(args[0]);
      return false;
    }
    return true;
  }
  if (name == "sleep" && argCount >= 1) {
    if (!ctx.statementOnly) {
      ctx.error = "sleep 只能作为独立语句使用";
      return false;
    }
    runtime.wakeAtMs = ctx.nowMs + (unsigned long)(max(scriptAsFloat(args[0]), 0.0f) * 1000.0f);
    ctx.pauseRequested = true;
    outValue = makeNullValue();
    return true;
  }
  if (name == "relay" && argCount >= 2) {
    ChannelConfig* channel = findChannel(scriptAsString(args[0]));
    if (channel == nullptr) {
      ctx.error = "未知继电器目标: " + scriptAsString(args[0]);
      return false;
    }
    applyRelay(channel->id, scriptAsBool(args[1]) || scriptAsString(args[1]) == "on" ? "on" : "off");
    channel->currentMode = "script";
    channel->currentStatus = "ok";
    outValue = args[1];
    return true;
  }
  if (name == "relay_on" && argCount >= 1) {
    ChannelConfig* channel = findChannel(scriptAsString(args[0]));
    if (channel == nullptr) {
      ctx.error = "未知继电器目标: " + scriptAsString(args[0]);
      return false;
    }
    applyRelay(channel->id, "on");
    channel->currentMode = "script";
    channel->currentStatus = "ok";
    outValue = makeStringValue("on");
    return true;
  }
  if (name == "relay_off" && argCount >= 1) {
    ChannelConfig* channel = findChannel(scriptAsString(args[0]));
    if (channel == nullptr) {
      ctx.error = "未知继电器目标: " + scriptAsString(args[0]);
      return false;
    }
    applyRelay(channel->id, "off");
    channel->currentMode = "script";
    channel->currentStatus = "ok";
    outValue = makeStringValue("off");
    return true;
  }
  if (name == "relay_toggle" && argCount >= 1) {
    ChannelConfig* channel = findChannel(scriptAsString(args[0]));
    if (channel == nullptr) {
      ctx.error = "未知继电器目标: " + scriptAsString(args[0]);
      return false;
    }
    applyRelayToggle(channel->id);
    channel->currentMode = "script";
    channel->currentStatus = "ok";
    outValue = makeStringValue(channel->currentState);
    return true;
  }
  if (name == "pwm" && argCount >= 2) {
    ChannelConfig* channel = findChannel(scriptAsString(args[0]));
    if (channel == nullptr) {
      ctx.error = "未知 PWM 目标: " + scriptAsString(args[0]);
      return false;
    }
    clearPWMRuntime(channel->id);
    writePWM(*channel, (int)lroundf(scriptAsFloat(args[1])));
    channel->currentMode = "script";
    channel->currentStatus = "ok";
    outValue = args[1];
    return true;
  }
  if (name == "pwm_direct" && argCount >= 2) {
    applyDirectPWM(scriptAsString(args[0]), (int)lroundf(scriptAsFloat(args[1])));
    ChannelConfig* channel = findChannel(scriptAsString(args[0]));
    if (channel != nullptr) {
      channel->currentStatus = "ok";
    }
    outValue = args[1];
    return true;
  }
  if (name == "pwm_stop" && argCount >= 1) {
    applyDirectPWM(scriptAsString(args[0]), 0);
    ChannelConfig* channel = findChannel(scriptAsString(args[0]));
    if (channel != nullptr) {
      channel->currentMode = "stop";
      channel->currentStatus = "ok";
    }
    outValue = makeNumberValue(0);
    return true;
  }
  if (name == "pwm_linear" && argCount >= 5) {
    applyLinearRamp(scriptAsString(args[0]), (int)lroundf(scriptAsFloat(args[1])), (int)lroundf(scriptAsFloat(args[2])), (int)lroundf(scriptAsFloat(args[3])), "linear", (int)lroundf(scriptAsFloat(args[4])));
    outValue = makeNullValue();
    return true;
  }
  if (name == "pwm_curve" && argCount >= 7) {
    applyCurveWave(scriptAsString(args[0]), (int)lroundf(scriptAsFloat(args[1])), (int)lroundf(scriptAsFloat(args[2])), (int)lroundf(scriptAsFloat(args[2])), (int)lroundf(scriptAsFloat(args[1])), (int)lroundf(scriptAsFloat(args[3])), scriptAsString(args[4]), scriptAsString(args[5]), (int)lroundf(scriptAsFloat(args[6])));
    outValue = makeNullValue();
    return true;
  }
  if (name == "pwm_sine" && argCount >= 5) {
    applySineWave(scriptAsString(args[0]), (int)lroundf(scriptAsFloat(args[1])), (int)lroundf(scriptAsFloat(args[2])), (int)lroundf(scriptAsFloat(args[3])), (int)lroundf(scriptAsFloat(args[4])));
    outValue = makeNullValue();
    return true;
  }
  if (name == "pwm_bezier" && argCount >= 7) {
    applyBezierWave(scriptAsString(args[0]), (int)lroundf(scriptAsFloat(args[1])), (int)lroundf(scriptAsFloat(args[2])), (int)lroundf(scriptAsFloat(args[3])), (int)lroundf(scriptAsFloat(args[4])), (int)lroundf(scriptAsFloat(args[5])), (int)lroundf(scriptAsFloat(args[6])));
    outValue = makeNullValue();
    return true;
  }
  if (name == "pwm_random" && argCount >= 6) {
    applyRandomWave(scriptAsString(args[0]), (int)lroundf(scriptAsFloat(args[1])), (int)lroundf(scriptAsFloat(args[2])), (int)lroundf(scriptAsFloat(args[3])), (int)lroundf(scriptAsFloat(args[4])), (int)lroundf(scriptAsFloat(args[5])));
    outValue = makeNullValue();
    return true;
  }
  if (name == "pwm_pulse" && argCount >= 6) {
    applyPulseWave(scriptAsString(args[0]), (int)lroundf(scriptAsFloat(args[1])), (int)lroundf(scriptAsFloat(args[2])), (int)lroundf(scriptAsFloat(args[3])), (int)lroundf(scriptAsFloat(args[4])), (int)lroundf(scriptAsFloat(args[5])));
    outValue = makeNullValue();
    return true;
  }
  if (name == "metric" && argCount >= 2) {
    writeScriptMetric(scriptAsString(args[0]), scriptAsFloat(args[1]));
    outValue = args[1];
    return true;
  }
  if (name == "status" && argCount >= 2) {
    ChannelConfig* channel = findChannel(scriptAsString(args[0]));
    if (channel == nullptr) {
      ctx.error = "未知状态目标: " + scriptAsString(args[0]);
      return false;
    }
    channel->currentStatus = scriptAsString(args[1]);
    outValue = args[1];
    return true;
  }

  ctx.error = "不支持的函数: " + name;
  return false;
}

bool scriptParsePrimary(ScriptRuntimeState& runtime, ScriptEvalContext& ctx, ScriptValue& outValue);

bool scriptParsePostfix(ScriptRuntimeState& runtime, ScriptEvalContext& ctx, ScriptValue& outValue) {
  return scriptParsePrimary(runtime, ctx, outValue);
}

bool scriptParseUnary(ScriptRuntimeState& runtime, ScriptEvalContext& ctx, ScriptValue& outValue) {
  if (scriptMatchToken(ctx, SCRIPT_TOKEN_OP, "-")) {
    ScriptValue inner;
    if (!scriptParseUnary(runtime, ctx, inner)) {
      return false;
    }
    outValue = makeNumberValue(-scriptAsFloat(inner));
    return true;
  }
  return scriptParsePostfix(runtime, ctx, outValue);
}

bool scriptParseProduct(ScriptRuntimeState& runtime, ScriptEvalContext& ctx, ScriptValue& outValue) {
  if (!scriptParseUnary(runtime, ctx, outValue)) {
    return false;
  }
  while (scriptCheckToken(ctx, SCRIPT_TOKEN_OP, "*") || scriptCheckToken(ctx, SCRIPT_TOKEN_OP, "/")) {
    String op = ctx.tokens[ctx.pos].value;
    ctx.pos += 1;
    ScriptValue right;
    if (!scriptParseUnary(runtime, ctx, right)) {
      return false;
    }
    if (op == "*") {
      outValue = makeNumberValue(scriptAsFloat(outValue) * scriptAsFloat(right));
    } else {
      float divisor = scriptAsFloat(right);
      outValue = makeNumberValue(fabsf(divisor) < 0.00001f ? 0.0f : scriptAsFloat(outValue) / divisor);
    }
  }
  return true;
}

bool scriptParseSum(ScriptRuntimeState& runtime, ScriptEvalContext& ctx, ScriptValue& outValue) {
  if (!scriptParseProduct(runtime, ctx, outValue)) {
    return false;
  }
  while (scriptCheckToken(ctx, SCRIPT_TOKEN_OP, "+") || scriptCheckToken(ctx, SCRIPT_TOKEN_OP, "-")) {
    String op = ctx.tokens[ctx.pos].value;
    ctx.pos += 1;
    ScriptValue right;
    if (!scriptParseProduct(runtime, ctx, right)) {
      return false;
    }
    if (op == "+") {
      outValue = makeNumberValue(scriptAsFloat(outValue) + scriptAsFloat(right));
    } else {
      outValue = makeNumberValue(scriptAsFloat(outValue) - scriptAsFloat(right));
    }
  }
  return true;
}

bool scriptParseExpression(ScriptRuntimeState& runtime, ScriptEvalContext& ctx, ScriptValue& outValue) {
  if (!scriptParseSum(runtime, ctx, outValue)) {
    return false;
  }
  while (scriptCheckToken(ctx, SCRIPT_TOKEN_OP, ">") || scriptCheckToken(ctx, SCRIPT_TOKEN_OP, "<") || scriptCheckToken(ctx, SCRIPT_TOKEN_OP, ">=") || scriptCheckToken(ctx, SCRIPT_TOKEN_OP, "<=") || scriptCheckToken(ctx, SCRIPT_TOKEN_OP, "==") || scriptCheckToken(ctx, SCRIPT_TOKEN_OP, "!=")) {
    String op = ctx.tokens[ctx.pos].value;
    ctx.pos += 1;
    ScriptValue right;
    if (!scriptParseSum(runtime, ctx, right)) {
      return false;
    }
    if (op == ">") {
      outValue = makeBoolValue(scriptAsFloat(outValue) > scriptAsFloat(right));
    } else if (op == "<") {
      outValue = makeBoolValue(scriptAsFloat(outValue) < scriptAsFloat(right));
    } else if (op == ">=") {
      outValue = makeBoolValue(scriptAsFloat(outValue) >= scriptAsFloat(right));
    } else if (op == "<=") {
      outValue = makeBoolValue(scriptAsFloat(outValue) <= scriptAsFloat(right));
    } else if (op == "==") {
      outValue = makeBoolValue(scriptCompareEq(outValue, right));
    } else {
      outValue = makeBoolValue(!scriptCompareEq(outValue, right));
    }
  }
  return true;
}

bool scriptParsePrimary(ScriptRuntimeState& runtime, ScriptEvalContext& ctx, ScriptValue& outValue) {
  if (ctx.pos >= ctx.tokenCount) {
    ctx.error = "表达式意外结束";
    return false;
  }
  ScriptToken token = ctx.tokens[ctx.pos];
  ctx.pos += 1;
  if (token.kind == SCRIPT_TOKEN_NUMBER) {
    outValue = makeNumberValue(token.value.toFloat());
    return true;
  }
  if (token.kind == SCRIPT_TOKEN_STRING) {
    outValue = makeStringValue(token.value);
    return true;
  }
  if (token.kind == SCRIPT_TOKEN_IDENT) {
    if (token.value == "true") {
      outValue = makeBoolValue(true);
      return true;
    }
    if (token.value == "false") {
      outValue = makeBoolValue(false);
      return true;
    }
    if (scriptMatchToken(ctx, SCRIPT_TOKEN_PUNCT, "(")) {
      ScriptValue args[SCRIPT_MAX_ARGS];
      int argCount = 0;
      if (!scriptCheckToken(ctx, SCRIPT_TOKEN_PUNCT, ")")) {
        while (true) {
          if (argCount >= SCRIPT_MAX_ARGS) {
            ctx.error = "函数参数过多";
            return false;
          }
          if (!scriptParseExpression(runtime, ctx, args[argCount])) {
            return false;
          }
          argCount += 1;
          if (scriptMatchToken(ctx, SCRIPT_TOKEN_PUNCT, ")")) {
            break;
          }
          if (!scriptMatchToken(ctx, SCRIPT_TOKEN_PUNCT, ",")) {
            ctx.error = "缺少逗号或右括号";
            return false;
          }
        }
      } else {
        argCount = 0;
      }
      return scriptCallFunction(runtime, ctx, token.value, args, argCount, outValue);
    }
    ScriptValue resolved;
    if (resolveScriptValue(runtime, token.value, resolved)) {
      outValue = resolved;
      return true;
    }
    outValue = makeStringValue(token.value);
    return true;
  }
  if (token.kind == SCRIPT_TOKEN_PUNCT && token.value == "(") {
    if (!scriptParseExpression(runtime, ctx, outValue)) {
      return false;
    }
    if (!scriptMatchToken(ctx, SCRIPT_TOKEN_PUNCT, ")")) {
      ctx.error = "缺少右括号";
      return false;
    }
    return true;
  }
  ctx.error = "不支持的表达式";
  return false;
}

bool evalScriptExpression(ScriptRuntimeState& runtime, const String& expression, unsigned long nowMs, bool statementOnly, ScriptValue& outValue, bool& paused, String& error) {
  ScriptEvalContext ctx;
  ctx.tokenCount = 0;
  ctx.pos = 0;
  ctx.statementOnly = statementOnly;
  ctx.pauseRequested = false;
  ctx.nowMs = nowMs;
  ctx.error = "";
  if (!tokenizeScriptExpression(expression, ctx)) {
    error = ctx.error;
    return false;
  }
  if (!scriptParseExpression(runtime, ctx, outValue)) {
    error = ctx.error;
    return false;
  }
  if (ctx.pos != ctx.tokenCount) {
    error = "表达式尾部存在未解析内容";
    return false;
  }
  paused = ctx.pauseRequested;
  return true;
}

bool executeScriptBlock(ScriptRuntimeState& runtime, uint16_t firstIndex, uint16_t resumeIndex, unsigned long nowMs, String& error);

bool pushScriptFrame(ScriptRuntimeState& runtime, uint16_t stmtIndex) {
  if (runtime.resumeCount >= SCRIPT_MAX_STACK) {
    return false;
  }
  runtime.resume[runtime.resumeCount].stmtIndex = stmtIndex;
  runtime.resumeCount += 1;
  return true;
}

bool executeScriptBlock(ScriptRuntimeState& runtime, uint16_t firstIndex, uint16_t resumeIndex, unsigned long nowMs, String& error) {
  uint16_t currentIndex = resumeIndex != 0xFFFF ? resumeIndex : firstIndex;
  while (currentIndex != 0xFFFF) {
    ScriptStmt& stmt = runtime.program.stmts[currentIndex];
    uint16_t nextIndex = stmt.nextIndex;
    if (stmt.kind == SCRIPT_STMT_CONST || stmt.kind == SCRIPT_STMT_VAR || stmt.kind == SCRIPT_STMT_ASSIGN) {
      ScriptValue value;
      bool paused = false;
      if (!evalScriptExpression(runtime, stmt.expr, nowMs, false, value, paused, error)) {
        error = "line " + String(stmt.line) + ": " + error;
        return false;
      }
      bool ok = stmt.kind == SCRIPT_STMT_CONST
        ? setScriptNamedValue(runtime.consts, SCRIPT_MAX_VALUES, stmt.name, value)
        : setScriptNamedValue(runtime.vars, SCRIPT_MAX_VALUES, stmt.name, value);
      if (!ok) {
        error = "line " + String(stmt.line) + ": 变量数量超限";
        return false;
      }
      currentIndex = nextIndex;
      continue;
    }
    if (stmt.kind == SCRIPT_STMT_CALL) {
      ScriptValue ignored;
      bool paused = false;
      if (!evalScriptExpression(runtime, stmt.expr, nowMs, true, ignored, paused, error)) {
        error = "line " + String(stmt.line) + ": " + error;
        return false;
      }
      if (paused) {
        if (nextIndex != 0xFFFF && !pushScriptFrame(runtime, nextIndex)) {
          error = "line " + String(stmt.line) + ": 脚本调用栈超限";
          return false;
        }
        return true;
      }
      currentIndex = nextIndex;
      continue;
    }
    if (stmt.kind == SCRIPT_STMT_IF) {
      ScriptValue condition;
      bool paused = false;
      if (!evalScriptExpression(runtime, stmt.expr, nowMs, false, condition, paused, error)) {
        error = "line " + String(stmt.line) + ": " + error;
        return false;
      }
      uint16_t childFirst = scriptAsBool(condition) ? stmt.bodyFirst : stmt.elseFirst;
      if (childFirst == 0xFFFF) {
        currentIndex = nextIndex;
        continue;
      }
      uint8_t resumeBefore = runtime.resumeCount;
      if (!executeScriptBlock(runtime, childFirst, 0xFFFF, nowMs, error)) {
        return false;
      }
      if (runtime.resumeCount > resumeBefore) {
        if (nextIndex != 0xFFFF && !pushScriptFrame(runtime, nextIndex)) {
          error = "line " + String(stmt.line) + ": 脚本调用栈超限";
          return false;
        }
        return true;
      }
    }
    currentIndex = nextIndex;
  }
  return true;
}

bool scriptShouldRun(const ScriptRuntimeState& runtime, unsigned long nowMs) {
  if (!runtime.loaded) {
    return false;
  }
  if (runtime.wakeAtMs > 0 && (long)(runtime.wakeAtMs - nowMs) > 0) {
    return false;
  }
  return runtime.lastRunAtMs == 0 || nowMs - runtime.lastRunAtMs >= 1000UL;
}

bool startScriptRuntime(const String& targetId, const String& source) {
  ScriptRuntimeState* runtime = scriptRuntimeForTarget(targetId);
  if (runtime == nullptr) {
    return false;
  }
  resetScriptRuntimeState(*runtime);
  runtime->targetId = targetId;
  String error;
  if (!parseUserScriptProgram(source, runtime->program, error)) {
    runtime->lastError = error;
    return false;
  }
  runtime->loaded = true;
  ChannelConfig* channel = findChannel(targetId);
  if (channel != nullptr) {
    channel->scriptSource = source;
    channel->currentStatus = "ok";
    if (channel->kind == CHANNEL_RELAY || channel->kind == CHANNEL_MOS_PWM) {
      channel->currentMode = "script";
    }
  }
  if (runtime->program.initBlock.first != 0xFFFF) {
    if (!executeScriptBlock(*runtime, runtime->program.initBlock.first, 0xFFFF, millis(), error)) {
      runtime->loaded = false;
      runtime->lastError = error;
      if (channel != nullptr) {
        channel->currentStatus = "script_error";
      }
      return false;
    }
  }
  runtime->lastRunAtMs = 0;
  return true;
}

void runScriptRuntimes() {
  unsigned long nowMs = millis();
  for (size_t i = 0; i < channelCount && i < 8; i++) {
    ChannelConfig& channel = channels[i];
    ScriptRuntimeState& runtime = scriptRuntimes[i];
    if (!runtime.loaded || channel.currentMode != "script") {
      continue;
    }
    if (!scriptShouldRun(runtime, nowMs)) {
      continue;
    }
    String error;
    if (runtime.resumeCount > 0) {
      ScriptFrame frame = runtime.resume[0];
      for (uint8_t shift = 1; shift < runtime.resumeCount; shift++) {
        runtime.resume[shift - 1] = runtime.resume[shift];
      }
      runtime.resumeCount -= 1;
      if (!executeScriptBlock(runtime, frame.stmtIndex, frame.stmtIndex, nowMs, error)) {
        runtime.lastError = error;
        channel.currentStatus = "script_error";
        continue;
      }
    } else if (runtime.program.loopBlock.first != 0xFFFF) {
      runtime.resumeCount = 0;
      if (!executeScriptBlock(runtime, runtime.program.loopBlock.first, 0xFFFF, nowMs, error)) {
        runtime.lastError = error;
        channel.currentStatus = "script_error";
        continue;
      }
    }
    if (runtime.wakeAtMs > 0 && (long)(nowMs - runtime.wakeAtMs) >= 0) {
      runtime.wakeAtMs = 0;
    }
    runtime.lastRunAtMs = nowMs;
  }
}
