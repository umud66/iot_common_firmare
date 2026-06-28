package main

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode"
)

type scriptProgram struct {
	init []scriptStmt
	loop []scriptStmt
}

type scriptStmt struct {
	kind     string
	name     string
	expr     *scriptExpr
	body     []scriptStmt
	elseBody []scriptStmt
	args     []scriptExpr
	line     int
}

type scriptExpr struct {
	kind  string
	value any
	name  string
	op    string
	left  *scriptExpr
	right *scriptExpr
	args  []scriptExpr
}

type scriptLine struct {
	indent int
	text   string
	line   int
}

type scriptParser struct {
	lines []scriptLine
	pos   int
}

type scriptToken struct {
	kind  string
	value string
}

type scriptExprParser struct {
	tokens []scriptToken
	pos    int
}

type scriptRuntime struct {
	program   scriptProgram
	consts    map[string]any
	vars      map[string]any
	lastRunAt time.Time
	wakeAt    time.Time
	resume    []scriptFrame
	randState uint64
}

type scriptFrame struct {
	items []scriptStmt
	index int
}

func loadScriptProgram(config map[string]any) (*scriptProgram, error) {
	if config == nil {
		return nil, nil
	}
	raw, ok := config["scriptSource"].(string)
	if !ok || strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	return parseUserScript(raw)
}

func newScriptRuntime(program scriptProgram, s *simulator) (*scriptRuntime, error) {
	runtime := &scriptRuntime{
		program:   program,
		consts:    map[string]any{},
		vars:      map[string]any{},
		randState: 0x9e3779b97f4a7c15,
	}
	if _, err := runtime.executeStatements(s, program.init, time.Now().UTC(), 0); err != nil {
		return nil, err
	}
	return runtime, nil
}

func (r *scriptRuntime) shouldRun(now time.Time) bool {
	if !r.wakeAt.IsZero() && now.Before(r.wakeAt) {
		return false
	}
	return r.lastRunAt.IsZero() || now.Sub(r.lastRunAt) >= time.Second
}

func (r *scriptRuntime) executeTick(s *simulator, now time.Time) error {
	var (
		frames []scriptFrame
		err    error
	)
	if len(r.resume) > 0 {
		frames, err = r.executeFrames(s, r.resume, now)
	} else {
		frames, err = r.executeStatements(s, r.program.loop, now, 0)
	}
	if err != nil {
		return err
	}
	r.resume = frames
	if !r.wakeAt.IsZero() && !now.Before(r.wakeAt) {
		r.wakeAt = time.Time{}
	}
	r.lastRunAt = now
	return nil
}

func (r *scriptRuntime) executeFrames(s *simulator, frames []scriptFrame, now time.Time) ([]scriptFrame, error) {
	if len(frames) == 0 {
		return nil, nil
	}
	current := frames[0]
	rest := frames[1:]
	pending, err := r.executeStatements(s, current.items, now, current.index)
	if err != nil {
		return nil, err
	}
	if len(pending) > 0 {
		return append(pending, rest...), nil
	}
	if len(rest) == 0 {
		return nil, nil
	}
	return r.executeFrames(s, rest, now)
}

func (r *scriptRuntime) executeStatements(s *simulator, items []scriptStmt, now time.Time, start int) ([]scriptFrame, error) {
	for index := start; index < len(items); index += 1 {
		item := items[index]
		switch item.kind {
		case "const":
			value, err := r.evalExpr(s, *item.expr, now)
			if err != nil {
				return nil, fmt.Errorf("line %d: %w", item.line, err)
			}
			r.consts[item.name] = value
		case "var":
			value, err := r.evalExpr(s, *item.expr, now)
			if err != nil {
				return nil, fmt.Errorf("line %d: %w", item.line, err)
			}
			r.vars[item.name] = value
		case "assign":
			value, err := r.evalExpr(s, *item.expr, now)
			if err != nil {
				return nil, fmt.Errorf("line %d: %w", item.line, err)
			}
			r.vars[item.name] = value
		case "if":
			condition, err := r.evalExpr(s, *item.expr, now)
			if err != nil {
				return nil, fmt.Errorf("line %d: %w", item.line, err)
			}
			if asBool(condition) {
				frames, err := r.executeStatements(s, item.body, now, 0)
				if err != nil {
					return nil, err
				}
				if len(frames) > 0 {
					return append(frames, scriptFrame{items: items, index: index + 1}), nil
				}
			} else if len(item.elseBody) > 0 {
				frames, err := r.executeStatements(s, item.elseBody, now, 0)
				if err != nil {
					return nil, err
				}
				if len(frames) > 0 {
					return append(frames, scriptFrame{items: items, index: index + 1}), nil
				}
			}
		case "call":
			paused, err := r.callStatement(s, item.name, item.args, now)
			if err != nil {
				return nil, fmt.Errorf("line %d: %w", item.line, err)
			}
			if paused {
				return []scriptFrame{{items: items, index: index + 1}}, nil
			}
		default:
			return nil, fmt.Errorf("line %d: unsupported statement %s", item.line, item.kind)
		}
	}
	return nil, nil
}

func (r *scriptRuntime) callStatement(s *simulator, name string, args []scriptExpr, now time.Time) (bool, error) {
	if name == "sleep" {
		values := make([]any, 0, len(args))
		for _, arg := range args {
			value, err := r.evalExpr(s, arg, now)
			if err != nil {
				return false, err
			}
			values = append(values, value)
		}
		seconds := math.Max(asFloat(values[0]), 0)
		r.wakeAt = now.Add(time.Duration(seconds * float64(time.Second)))
		return true, nil
	}
	if _, err := r.callFunction(s, name, args, now, true); err != nil {
		return false, err
	}
	return false, nil
}

func (r *scriptRuntime) evalExpr(s *simulator, expr scriptExpr, now time.Time) (any, error) {
	switch expr.kind {
	case "literal":
		return expr.value, nil
	case "ident":
		if value, ok := r.vars[expr.name]; ok {
			return value, nil
		}
		if value, ok := r.consts[expr.name]; ok {
			return value, nil
		}
		return expr.name, nil
	case "array":
		result := make([]any, 0, len(expr.args))
		for _, item := range expr.args {
			value, err := r.evalExpr(s, item, now)
			if err != nil {
				return nil, err
			}
			result = append(result, value)
		}
		return result, nil
	case "index":
		left, err := r.evalExpr(s, *expr.left, now)
		if err != nil {
			return nil, err
		}
		indexValue, err := r.evalExpr(s, *expr.right, now)
		if err != nil {
			return nil, err
		}
		items, ok := left.([]any)
		if !ok {
			return nil, fmt.Errorf("index target is not array")
		}
		index := int(asFloat(indexValue))
		if index < 0 || index >= len(items) {
			return nil, fmt.Errorf("index out of range")
		}
		return items[index], nil
	case "unary":
		value, err := r.evalExpr(s, *expr.left, now)
		if err != nil {
			return nil, err
		}
		if expr.op == "-" {
			return -asFloat(value), nil
		}
		return nil, fmt.Errorf("unsupported unary op %s", expr.op)
	case "binary":
		left, err := r.evalExpr(s, *expr.left, now)
		if err != nil {
			return nil, err
		}
		right, err := r.evalExpr(s, *expr.right, now)
		if err != nil {
			return nil, err
		}
		switch expr.op {
		case "+":
			return asFloat(left) + asFloat(right), nil
		case "-":
			return asFloat(left) - asFloat(right), nil
		case "*":
			return asFloat(left) * asFloat(right), nil
		case "/":
			if asFloat(right) == 0 {
				return 0.0, nil
			}
			return asFloat(left) / asFloat(right), nil
		case ">":
			return asFloat(left) > asFloat(right), nil
		case "<":
			return asFloat(left) < asFloat(right), nil
		case ">=":
			return asFloat(left) >= asFloat(right), nil
		case "<=":
			return asFloat(left) <= asFloat(right), nil
		case "==":
			return compareEq(left, right), nil
		case "!=":
			return !compareEq(left, right), nil
		default:
			return nil, fmt.Errorf("unsupported op %s", expr.op)
		}
	case "call":
		return r.callFunction(s, expr.name, expr.args, now, false)
	default:
		return nil, fmt.Errorf("unsupported expr kind %s", expr.kind)
	}
}

func (r *scriptRuntime) callFunction(s *simulator, name string, args []scriptExpr, now time.Time, statementOnly bool) (any, error) {
	values := make([]any, 0, len(args))
	for _, arg := range args {
		value, err := r.evalExpr(s, arg, now)
		if err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	switch name {
	case "min":
		return minArgs(values), nil
	case "max":
		return maxArgs(values), nil
	case "clamp":
		return clampFloat(asFloat(values[0]), asFloat(values[1]), asFloat(values[2])), nil
	case "abs":
		return math.Abs(asFloat(values[0])), nil
	case "floor":
		return math.Floor(asFloat(values[0])), nil
	case "ceil":
		return math.Ceil(asFloat(values[0])), nil
	case "sin":
		return math.Sin(asFloat(values[0])), nil
	case "cos":
		return math.Cos(asFloat(values[0])), nil
	case "pow":
		return math.Pow(asFloat(values[0]), asFloat(values[1])), nil
	case "norm":
		base := asFloat(values[0])
		minValue := asFloat(values[1])
		maxValue := asFloat(values[2])
		if maxValue == minValue {
			return 0.0, nil
		}
		return clampFloat((base-minValue)/(maxValue-minValue), 0, 1), nil
	case "map":
		return mapRange(asFloat(values[0]), asFloat(values[1]), asFloat(values[2]), asFloat(values[3]), asFloat(values[4])), nil
	case "wave":
		return clampFloat(mapRange(asFloat(values[0]), asFloat(values[1]), asFloat(values[2]), asFloat(values[3]), asFloat(values[4])), asFloat(values[3]), asFloat(values[4])), nil
	case "wrap_add":
		value := asFloat(values[0]) + asFloat(values[1])
		limit := asFloat(values[2])
		if limit == 0 {
			return 0.0, nil
		}
		for value > limit {
			value -= limit
		}
		return value, nil
	case "bezier":
		return cubicBezier(asFloat(values[0]), asFloat(values[1]), asFloat(values[2]), asFloat(values[3]), asFloat(values[4])), nil
	case "rand":
		return r.randomRange(asFloat(values[0]), asFloat(values[1])), nil
	case "read":
		return r.readValue(s, asString(values[0]), now)
	case "pwm":
		if statementOnly {
			return nil, r.writePWM(s, values[0], values[1])
		}
		return asFloat(values[1]), nil
	case "pwm_direct":
		if statementOnly {
			return nil, r.startPWMScriptRuntime(s, values[0], "direct", map[string]any{
				"duty": int(math.Round(asFloat(values[1]))),
			})
		}
		return asFloat(values[1]), nil
	case "pwm_linear":
		if statementOnly {
			return nil, r.startPWMScriptRuntime(s, values[0], "linearRamp", map[string]any{
				"from":       int(math.Round(asFloat(values[1]))),
				"to":         int(math.Round(asFloat(values[2]))),
				"durationMs": int(math.Round(asFloat(values[3]))),
				"loop":       int(math.Round(asFloat(values[4]))),
			})
		}
		return nil, nil
	case "pwm_curve":
		if statementOnly {
			return nil, r.startPWMScriptRuntime(s, values[0], "curveWave", map[string]any{
				"from":       int(math.Round(asFloat(values[1]))),
				"to":         int(math.Round(asFloat(values[2]))),
				"durationMs": int(math.Round(asFloat(values[3]))),
				"curve":      asString(values[4]),
				"direction":  asString(values[5]),
				"loop":       int(math.Round(asFloat(values[6]))),
			})
		}
		return nil, nil
	case "pwm_sine":
		if statementOnly {
			return nil, r.startPWMScriptRuntime(s, values[0], "sineWave", map[string]any{
				"minDuty":  int(math.Round(asFloat(values[1]))),
				"maxDuty":  int(math.Round(asFloat(values[2]))),
				"periodMs": int(math.Round(asFloat(values[3]))),
				"loop":     int(math.Round(asFloat(values[4]))),
			})
		}
		return nil, nil
	case "pwm_bezier":
		if statementOnly {
			return nil, r.startPWMScriptRuntime(s, values[0], "bezierWave", map[string]any{
				"from":       int(math.Round(asFloat(values[1]))),
				"control1":   int(math.Round(asFloat(values[2]))),
				"control2":   int(math.Round(asFloat(values[3]))),
				"to":         int(math.Round(asFloat(values[4]))),
				"durationMs": int(math.Round(asFloat(values[5]))),
				"loop":       int(math.Round(asFloat(values[6]))),
			})
		}
		return nil, nil
	case "pwm_random":
		if statementOnly {
			return nil, r.startPWMScriptRuntime(s, values[0], "randomWave", map[string]any{
				"minDuty":    int(math.Round(asFloat(values[1]))),
				"maxDuty":    int(math.Round(asFloat(values[2]))),
				"intervalMs": int(math.Round(asFloat(values[3]))),
				"smoothing":  int(math.Round(asFloat(values[4]))),
				"loop":       int(math.Round(asFloat(values[5]))),
			})
		}
		return nil, nil
	case "pwm_pulse":
		if statementOnly {
			return nil, r.startPWMScriptRuntime(s, values[0], "pulseWave", map[string]any{
				"lowDuty":       int(math.Round(asFloat(values[1]))),
				"highDuty":      int(math.Round(asFloat(values[2]))),
				"onDurationMs":  int(math.Round(asFloat(values[3]))),
				"offDurationMs": int(math.Round(asFloat(values[4]))),
				"loop":          int(math.Round(asFloat(values[5]))),
			})
		}
		return nil, nil
	case "pwm_stop":
		if statementOnly {
			return nil, r.startPWMScriptRuntime(s, values[0], "stop", nil)
		}
		return nil, nil
	case "relay":
		if statementOnly {
			return nil, r.writeRelay(s, values[0], values[1])
		}
		return values[1], nil
	case "relay_on":
		if statementOnly {
			return nil, r.writeRelay(s, values[0], "on")
		}
		return "on", nil
	case "relay_off":
		if statementOnly {
			return nil, r.writeRelay(s, values[0], "off")
		}
		return "off", nil
	case "relay_toggle":
		if statementOnly {
			channel := s.channels[asString(values[0])]
			if channel == nil {
				return nil, fmt.Errorf("unknown relay target %s", asString(values[0]))
			}
			nextState := "on"
			if channel.State == "on" {
				nextState = "off"
			}
			return nil, r.writeRelay(s, values[0], nextState)
		}
		return nil, nil
	case "metric":
		if statementOnly {
			return nil, r.writeMetric(s, values[0], values[1])
		}
		return asFloat(values[1]), nil
	case "status":
		if statementOnly {
			return nil, r.writeStatus(s, values[0], values[1])
		}
		return asString(values[1]), nil
	case "sleep":
		return nil, fmt.Errorf("sleep 只能作为独立语句使用")
	default:
		return nil, fmt.Errorf("unsupported function %s", name)
	}
}

func (r *scriptRuntime) startPWMScriptRuntime(s *simulator, target any, mode string, params map[string]any) error {
	targetID := asString(target)
	channel := s.channels[targetID]
	if channel == nil {
		return fmt.Errorf("unknown pwm target %s", targetID)
	}
	if s.pwmRuntimes == nil {
		s.pwmRuntimes = map[string]*pwmRuntime{}
	}
	switch mode {
	case "direct":
		delete(s.pwmRuntimes, targetID)
		return s.writePWM(channel, intValue(params, "duty", channel.Duty), "direct")
	case "stop":
		delete(s.pwmRuntimes, targetID)
		return s.writePWM(channel, 0, "stop")
	case "linearRamp":
		s.pwmRuntimes[targetID] = &pwmRuntime{Mode: "linearRamp", Params: cloneParams(params), StartedAt: time.Now().UTC()}
		return s.writePWM(channel, intValue(params, "from", channel.Duty), "linearRamp")
	case "curveWave":
		s.pwmRuntimes[targetID] = &pwmRuntime{Mode: "curveWave", Params: cloneParams(params), StartedAt: time.Now().UTC()}
		return s.writePWM(channel, intValue(params, "from", channel.Duty), "curveWave")
	case "sineWave":
		s.pwmRuntimes[targetID] = &pwmRuntime{Mode: "sineWave", Params: cloneParams(params), StartedAt: time.Now().UTC()}
		return s.writePWM(channel, intValue(params, "minDuty", channel.Duty), "sineWave")
	case "bezierWave":
		s.pwmRuntimes[targetID] = &pwmRuntime{Mode: "bezierWave", Params: cloneParams(params), StartedAt: time.Now().UTC()}
		return s.writePWM(channel, intValue(params, "from", channel.Duty), "bezierWave")
	case "randomWave":
		s.pwmRuntimes[targetID] = &pwmRuntime{
			Mode:        "randomWave",
			Params:      cloneParams(params),
			StartedAt:   time.Now().UTC(),
			CurrentDuty: float64(channel.Duty),
		}
		return s.writePWM(channel, intValue(params, "minDuty", channel.Duty), "randomWave")
	case "pulseWave":
		s.pwmRuntimes[targetID] = &pwmRuntime{Mode: "pulseWave", Params: cloneParams(params), StartedAt: time.Now().UTC()}
		return s.writePWM(channel, intValue(params, "highDuty", channel.Duty), "pulseWave")
	default:
		return fmt.Errorf("unsupported pwm helper mode %s", mode)
	}
}

func (r *scriptRuntime) randomRange(minValue, maxValue float64) float64 {
	if maxValue < minValue {
		minValue, maxValue = maxValue, minValue
	}
	if minValue == maxValue {
		return minValue
	}
	return minValue + r.nextRandom()*(maxValue-minValue)
}

func (r *scriptRuntime) nextRandom() float64 {
	if r.randState == 0 {
		r.randState = 0x9e3779b97f4a7c15
	}
	r.randState ^= r.randState >> 12
	r.randState ^= r.randState << 25
	r.randState ^= r.randState >> 27
	return float64((r.randState*2685821657736338717)>>11) / float64(uint64(1)<<53)
}

func (r *scriptRuntime) readValue(s *simulator, key string, now time.Time) (any, error) {
	parts := strings.Split(key, ".")
	if len(parts) == 2 {
		channel := s.channels[parts[0]]
		if channel != nil {
			switch parts[1] {
			case "duty":
				return float64(channel.Duty), nil
			case "temperatureC":
				return channel.TemperatureC, nil
			case "state":
				return channel.State, nil
			case "mode":
				return channel.Mode, nil
			case "status":
				return channel.Status, nil
			}
		}
		if parts[0] == "system" {
			switch parts[1] {
			case "uptimeSec":
				return time.Since(s.startedAt).Seconds(), nil
			case "tickSec":
				return float64(now.Unix()), nil
			}
		}
	}
	return nil, fmt.Errorf("unsupported read target %s", key)
}

func (r *scriptRuntime) writePWM(s *simulator, target any, value any) error {
	channel := s.channels[asString(target)]
	if channel == nil {
		return fmt.Errorf("unknown pwm target %s", asString(target))
	}
	return s.writePWM(channel, int(math.Round(asFloat(value))), "script")
}

func (r *scriptRuntime) writeRelay(s *simulator, target any, value any) error {
	channel := s.channels[asString(target)]
	if channel == nil {
		return fmt.Errorf("unknown relay target %s", asString(target))
	}
	state := "off"
	if asBool(value) || asString(value) == "on" {
		state = "on"
	}
	return s.writeRelay(channel, state, "script")
}

func (r *scriptRuntime) writeMetric(s *simulator, target any, value any) error {
	if s.metrics == nil {
		s.metrics = map[string]float64{}
	}
	s.metrics[asString(target)] = asFloat(value)
	return nil
}

func (r *scriptRuntime) writeStatus(s *simulator, target any, value any) error {
	channel := s.channels[asString(target)]
	if channel == nil {
		return fmt.Errorf("unknown status target %s", asString(target))
	}
	channel.Status = asString(value)
	return nil
}

func parseUserScript(source string) (*scriptProgram, error) {
	lines := normalizeUserScriptLines(source)
	parser := &scriptParser{lines: lines}
	program := &scriptProgram{}
	for parser.pos < len(parser.lines) {
		line := parser.lines[parser.pos]
		if strings.TrimSpace(line.text) == "loop:" {
			parser.pos++
			body, err := parser.parseBlock(line.indent + 2)
			if err != nil {
				return nil, err
			}
			program.loop = body
			continue
		}
		stmt, err := parseUserScriptStatement(line)
		if err != nil {
			return nil, err
		}
		parser.pos++
		if stmt.kind == "if" {
			body, err := parser.parseBlock(line.indent + 2)
			if err != nil {
				return nil, err
			}
			stmt.body = body
			if parser.pos < len(parser.lines) && parser.lines[parser.pos].indent == line.indent && strings.TrimSpace(parser.lines[parser.pos].text) == "else:" {
				parser.pos++
				elseBody, err := parser.parseBlock(line.indent + 2)
				if err != nil {
					return nil, err
				}
				stmt.elseBody = elseBody
			}
		}
		program.init = append(program.init, stmt)
	}
	return program, nil
}

func (p *scriptParser) parseBlock(indent int) ([]scriptStmt, error) {
	result := []scriptStmt{}
	for p.pos < len(p.lines) {
		line := p.lines[p.pos]
		if line.indent < indent {
			break
		}
		if line.indent > indent {
			return nil, fmt.Errorf("line %d: invalid indentation", line.line)
		}
		if strings.TrimSpace(line.text) == "else:" {
			break
		}
		stmt, err := parseUserScriptStatement(line)
		if err != nil {
			return nil, err
		}
		p.pos++
		if stmt.kind == "if" {
			body, err := p.parseBlock(indent + 2)
			if err != nil {
				return nil, err
			}
			stmt.body = body
			if p.pos < len(p.lines) && p.lines[p.pos].indent == indent && strings.TrimSpace(p.lines[p.pos].text) == "else:" {
				p.pos++
				elseBody, err := p.parseBlock(indent + 2)
				if err != nil {
					return nil, err
				}
				stmt.elseBody = elseBody
			}
		}
		result = append(result, stmt)
	}
	return result, nil
}

func parseUserScriptStatement(line scriptLine) (scriptStmt, error) {
	text := strings.TrimSpace(line.text)
	switch {
	case strings.HasPrefix(text, "const "):
		name, expr, err := splitUserAssignment(strings.TrimPrefix(text, "const "))
		if err != nil {
			return scriptStmt{}, fmt.Errorf("line %d: %w", line.line, err)
		}
		parsed, err := parseUserScriptExpression(expr)
		if err != nil {
			return scriptStmt{}, fmt.Errorf("line %d: %w", line.line, err)
		}
		return scriptStmt{kind: "const", name: name, expr: &parsed, line: line.line}, nil
	case strings.HasPrefix(text, "var "):
		name, expr, err := splitUserAssignment(strings.TrimPrefix(text, "var "))
		if err != nil {
			return scriptStmt{}, fmt.Errorf("line %d: %w", line.line, err)
		}
		parsed, err := parseUserScriptExpression(expr)
		if err != nil {
			return scriptStmt{}, fmt.Errorf("line %d: %w", line.line, err)
		}
		return scriptStmt{kind: "var", name: name, expr: &parsed, line: line.line}, nil
	case strings.HasPrefix(text, "if ") && strings.HasSuffix(text, ":"):
		parsed, err := parseUserScriptExpression(strings.TrimSuffix(strings.TrimSpace(strings.TrimPrefix(text, "if ")), ":"))
		if err != nil {
			return scriptStmt{}, fmt.Errorf("line %d: %w", line.line, err)
		}
		return scriptStmt{kind: "if", expr: &parsed, line: line.line}, nil
	case strings.Contains(text, "="):
		name, expr, err := splitUserAssignment(text)
		if err != nil {
			return scriptStmt{}, fmt.Errorf("line %d: %w", line.line, err)
		}
		parsed, err := parseUserScriptExpression(expr)
		if err != nil {
			return scriptStmt{}, fmt.Errorf("line %d: %w", line.line, err)
		}
		return scriptStmt{kind: "assign", name: name, expr: &parsed, line: line.line}, nil
	default:
		expr, err := parseUserScriptExpression(text)
		if err != nil {
			return scriptStmt{}, fmt.Errorf("line %d: %w", line.line, err)
		}
		if expr.kind != "call" {
			return scriptStmt{}, fmt.Errorf("line %d: only function calls allowed", line.line)
		}
		return scriptStmt{kind: "call", name: expr.name, args: expr.args, line: line.line}, nil
	}
}

func splitUserAssignment(text string) (string, string, error) {
	parts := strings.SplitN(text, "=", 2)
	if len(parts) != 2 {
		return "", "", fmt.Errorf("missing =")
	}
	name := strings.TrimSpace(parts[0])
	if name == "" {
		return "", "", fmt.Errorf("variable name is empty")
	}
	return name, strings.TrimSpace(parts[1]), nil
}

func normalizeUserScriptLines(source string) []scriptLine {
	rawLines := strings.Split(strings.ReplaceAll(source, "\r\n", "\n"), "\n")
	result := make([]scriptLine, 0, len(rawLines))
	for index, raw := range rawLines {
		trimmed := strings.TrimRight(raw, " \t")
		if strings.TrimSpace(trimmed) == "" {
			continue
		}
		noComment := trimmed
		if commentIndex := strings.Index(noComment, "#"); commentIndex >= 0 {
			noComment = strings.TrimRight(noComment[:commentIndex], " \t")
			if strings.TrimSpace(noComment) == "" {
				continue
			}
		}
		indent := 0
		for _, ch := range noComment {
			if ch == ' ' {
				indent++
				continue
			}
			break
		}
		result = append(result, scriptLine{indent: indent, text: noComment, line: index + 1})
	}
	return result
}

func parseUserScriptExpression(input string) (scriptExpr, error) {
	tokens, err := tokenizeUserScriptExpr(input)
	if err != nil {
		return scriptExpr{}, err
	}
	parser := &scriptExprParser{tokens: tokens}
	expr, err := parser.parseCompare()
	if err != nil {
		return scriptExpr{}, err
	}
	if parser.pos != len(parser.tokens) {
		return scriptExpr{}, fmt.Errorf("unexpected tail")
	}
	return expr, nil
}

func tokenizeUserScriptExpr(input string) ([]scriptToken, error) {
	tokens := []scriptToken{}
	for index := 0; index < len(input); {
		ch := rune(input[index])
		if unicode.IsSpace(ch) {
			index++
			continue
		}
		if unicode.IsDigit(ch) || ch == '.' {
			start := index
			index++
			for index < len(input) {
				next := rune(input[index])
				if unicode.IsDigit(next) || next == '.' {
					index++
					continue
				}
				break
			}
			tokens = append(tokens, scriptToken{kind: "number", value: input[start:index]})
			continue
		}
		if unicode.IsLetter(ch) || ch == '_' {
			start := index
			index++
			for index < len(input) {
				next := rune(input[index])
				if unicode.IsLetter(next) || unicode.IsDigit(next) || next == '_' || next == '.' {
					index++
					continue
				}
				break
			}
			tokens = append(tokens, scriptToken{kind: "ident", value: input[start:index]})
			continue
		}
		if ch == '"' {
			start := index + 1
			index++
			for index < len(input) && rune(input[index]) != '"' {
				index++
			}
			if index >= len(input) {
				return nil, fmt.Errorf("unterminated string")
			}
			tokens = append(tokens, scriptToken{kind: "string", value: input[start:index]})
			index++
			continue
		}
		if index+1 < len(input) {
			two := input[index : index+2]
			switch two {
			case ">=", "<=", "==", "!=":
				tokens = append(tokens, scriptToken{kind: "op", value: two})
				index += 2
				continue
			}
		}
		switch ch {
		case '+', '-', '*', '/', '>', '<', '(', ')', '[', ']', ',':
			kind := "op"
			if strings.ContainsRune("()[],", ch) {
				kind = "punct"
			}
			tokens = append(tokens, scriptToken{kind: kind, value: string(ch)})
			index++
		default:
			return nil, fmt.Errorf("unsupported char %q", string(ch))
		}
	}
	return tokens, nil
}

func (p *scriptExprParser) parseCompare() (scriptExpr, error) {
	left, err := p.parseSum()
	if err != nil {
		return scriptExpr{}, err
	}
	for p.matchOp(">", "<", ">=", "<=", "==", "!=") {
		op := p.prev().value
		right, err := p.parseSum()
		if err != nil {
			return scriptExpr{}, err
		}
		leftCopy := left
		rightCopy := right
		left = scriptExpr{kind: "binary", op: op, left: &leftCopy, right: &rightCopy}
	}
	return left, nil
}

func (p *scriptExprParser) parseSum() (scriptExpr, error) {
	left, err := p.parseProduct()
	if err != nil {
		return scriptExpr{}, err
	}
	for p.matchOp("+", "-") {
		op := p.prev().value
		right, err := p.parseProduct()
		if err != nil {
			return scriptExpr{}, err
		}
		leftCopy := left
		rightCopy := right
		left = scriptExpr{kind: "binary", op: op, left: &leftCopy, right: &rightCopy}
	}
	return left, nil
}

func (p *scriptExprParser) parseProduct() (scriptExpr, error) {
	left, err := p.parseUnary()
	if err != nil {
		return scriptExpr{}, err
	}
	for p.matchOp("*", "/") {
		op := p.prev().value
		right, err := p.parseUnary()
		if err != nil {
			return scriptExpr{}, err
		}
		leftCopy := left
		rightCopy := right
		left = scriptExpr{kind: "binary", op: op, left: &leftCopy, right: &rightCopy}
	}
	return left, nil
}

func (p *scriptExprParser) parseUnary() (scriptExpr, error) {
	if p.matchOp("-") {
		value, err := p.parseUnary()
		if err != nil {
			return scriptExpr{}, err
		}
		return scriptExpr{kind: "unary", op: "-", left: &value}, nil
	}
	return p.parsePostfix()
}

func (p *scriptExprParser) parsePostfix() (scriptExpr, error) {
	expr, err := p.parsePrimary()
	if err != nil {
		return scriptExpr{}, err
	}
	for {
		if p.matchPunct("(") {
			args := []scriptExpr{}
			if !p.checkPunct(")") {
				for {
					arg, err := p.parseCompare()
					if err != nil {
						return scriptExpr{}, err
					}
					args = append(args, arg)
					if !p.matchPunct(",") {
						break
					}
				}
			}
			if !p.matchPunct(")") {
				return scriptExpr{}, fmt.Errorf("missing )")
			}
			if expr.kind != "ident" {
				return scriptExpr{}, fmt.Errorf("only direct function names supported")
			}
			expr = scriptExpr{kind: "call", name: expr.name, args: args}
			continue
		}
		if p.matchPunct("[") {
			indexExpr, err := p.parseCompare()
			if err != nil {
				return scriptExpr{}, err
			}
			if !p.matchPunct("]") {
				return scriptExpr{}, fmt.Errorf("missing ]")
			}
			exprCopy := expr
			indexCopy := indexExpr
			expr = scriptExpr{kind: "index", left: &exprCopy, right: &indexCopy}
			continue
		}
		break
	}
	return expr, nil
}

func (p *scriptExprParser) parsePrimary() (scriptExpr, error) {
	if p.pos >= len(p.tokens) {
		return scriptExpr{}, fmt.Errorf("unexpected end")
	}
	token := p.tokens[p.pos]
	p.pos++
	switch token.kind {
	case "number":
		value, err := strconv.ParseFloat(token.value, 64)
		if err != nil {
			return scriptExpr{}, fmt.Errorf("invalid number %s", token.value)
		}
		return scriptExpr{kind: "literal", value: value}, nil
	case "string":
		return scriptExpr{kind: "literal", value: token.value}, nil
	case "ident":
		switch token.value {
		case "true":
			return scriptExpr{kind: "literal", value: true}, nil
		case "false":
			return scriptExpr{kind: "literal", value: false}, nil
		}
		return scriptExpr{kind: "ident", name: token.value}, nil
	case "punct":
		switch token.value {
		case "(":
			expr, err := p.parseCompare()
			if err != nil {
				return scriptExpr{}, err
			}
			if !p.matchPunct(")") {
				return scriptExpr{}, fmt.Errorf("missing )")
			}
			return expr, nil
		case "[":
			items := []scriptExpr{}
			if !p.checkPunct("]") {
				for {
					item, err := p.parseCompare()
					if err != nil {
						return scriptExpr{}, err
					}
					items = append(items, item)
					if !p.matchPunct(",") {
						break
					}
				}
			}
			if !p.matchPunct("]") {
				return scriptExpr{}, fmt.Errorf("missing ]")
			}
			return scriptExpr{kind: "array", args: items}, nil
		}
	}
	return scriptExpr{}, fmt.Errorf("unexpected token %s", token.value)
}

func (p *scriptExprParser) matchOp(values ...string) bool {
	if p.pos >= len(p.tokens) || p.tokens[p.pos].kind != "op" {
		return false
	}
	for _, item := range values {
		if p.tokens[p.pos].value == item {
			p.pos++
			return true
		}
	}
	return false
}

func (p *scriptExprParser) matchPunct(value string) bool {
	if p.pos >= len(p.tokens) || p.tokens[p.pos].kind != "punct" || p.tokens[p.pos].value != value {
		return false
	}
	p.pos++
	return true
}

func (p *scriptExprParser) checkPunct(value string) bool {
	return p.pos < len(p.tokens) && p.tokens[p.pos].kind == "punct" && p.tokens[p.pos].value == value
}

func (p *scriptExprParser) prev() scriptToken {
	return p.tokens[p.pos-1]
}

func mapRange(value float64, inMin float64, inMax float64, outMin float64, outMax float64) float64 {
	if inMax == inMin {
		return outMin
	}
	ratio := (value - inMin) / (inMax - inMin)
	return outMin + ratio*(outMax-outMin)
}

func cubicBezier(t float64, p0 float64, p1 float64, p2 float64, p3 float64) float64 {
	u := 1 - t
	return u*u*u*p0 + 3*u*u*t*p1 + 3*u*t*t*p2 + t*t*t*p3
}
