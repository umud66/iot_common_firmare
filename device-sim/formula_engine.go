package main

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"
)

type formulaProgram struct {
	Version string         `json:"version"`
	TickMs  int            `json:"tickMs,omitempty"`
	Params  map[string]any `json:"params,omitempty"`
	Vars    map[string]any `json:"vars,omitempty"`
	Rules   []formulaRule  `json:"rules,omitempty"`
}

type formulaRule struct {
	ID      string        `json:"id"`
	Enabled bool          `json:"enabled"`
	Target  formulaTarget `json:"target"`
	Expr    formulaExpr   `json:"expr"`
}

type formulaTarget struct {
	Kind  string `json:"kind"`
	Key   string `json:"key"`
	Field string `json:"field,omitempty"`
}

type formulaExpr struct {
	Value any           `json:"value,omitempty"`
	Ref   string        `json:"ref,omitempty"`
	Op    string        `json:"op,omitempty"`
	Args  []formulaExpr `json:"args,omitempty"`
}

type formulaRuntime struct {
	ownerTargetID string
	program       formulaProgram
	vars          map[string]any
	lastRunAt     time.Time
}

func loadFormulaProgram(config map[string]any) (*formulaProgram, error) {
	if config == nil {
		return nil, nil
	}
	raw, ok := config["formula"]
	if !ok || raw == nil {
		return nil, nil
	}
	payload, err := json.Marshal(raw)
	if err != nil {
		return nil, err
	}
	var program formulaProgram
	if err := json.Unmarshal(payload, &program); err != nil {
		return nil, err
	}
	if program.Version == "" || program.Version != "v1" {
		return nil, fmt.Errorf("unsupported formula version: %s", program.Version)
	}
	return &program, nil
}

func newFormulaRuntime(ownerTargetID string, program formulaProgram) *formulaRuntime {
	vars := map[string]any{}
	for key, value := range program.Vars {
		vars[key] = value
	}
	return &formulaRuntime{
		ownerTargetID: ownerTargetID,
		program:       program,
		vars:          vars,
	}
}

func (r *formulaRuntime) shouldRun(now time.Time) bool {
	interval := time.Second
	if r.program.TickMs > 0 {
		interval = time.Duration(r.program.TickMs) * time.Millisecond
	}
	return r.lastRunAt.IsZero() || now.Sub(r.lastRunAt) >= interval
}

func (r *formulaRuntime) execute(s *simulator, now time.Time) error {
	for _, rule := range r.program.Rules {
		if !rule.Enabled {
			continue
		}
		value, err := r.evalExpr(s, rule.Expr, now)
		if err != nil {
			return fmt.Errorf("rule %s: %w", firstNonEmpty(rule.ID, "<unnamed>"), err)
		}
		if err := r.applyTarget(s, rule.Target, value); err != nil {
			return fmt.Errorf("rule %s: %w", firstNonEmpty(rule.ID, "<unnamed>"), err)
		}
	}
	r.lastRunAt = now
	return nil
}

func (r *formulaRuntime) applyTarget(s *simulator, target formulaTarget, value any) error {
	switch target.Kind {
	case "var":
		r.vars[target.Key] = value
		return nil
	case "metric":
		if s.metrics == nil {
			s.metrics = map[string]float64{}
		}
		s.metrics[target.Key] = asFloat(value)
		return nil
	case "channel":
		channel := s.channels[target.Key]
		if channel == nil {
			return fmt.Errorf("unknown channel: %s", target.Key)
		}
		switch target.Field {
		case "duty":
			return s.writePWM(channel, int(math.Round(asFloat(value))), "")
		case "temperatureC":
			channel.TemperatureC = asFloat(value)
		case "state":
			return s.writeRelay(channel, asString(value), "")
		case "mode":
			channel.Mode = asString(value)
		case "status":
			channel.Status = asString(value)
		default:
			return fmt.Errorf("unsupported channel field: %s", target.Field)
		}
		return nil
	default:
		return fmt.Errorf("unsupported target kind: %s", target.Kind)
	}
}

func (r *formulaRuntime) evalExpr(s *simulator, expr formulaExpr, now time.Time) (any, error) {
	switch {
	case expr.Ref != "":
		return r.resolveRef(s, expr.Ref, now)
	case expr.Op != "":
		return r.evalOp(s, expr, now)
	default:
		return expr.Value, nil
	}
}

func (r *formulaRuntime) resolveRef(s *simulator, ref string, now time.Time) (any, error) {
	parts := strings.Split(ref, ".")
	switch parts[0] {
	case "var":
		if len(parts) != 2 {
			return nil, fmt.Errorf("invalid ref: %s", ref)
		}
		return r.vars[parts[1]], nil
	case "param":
		if len(parts) != 2 {
			return nil, fmt.Errorf("invalid ref: %s", ref)
		}
		return r.program.Params[parts[1]], nil
	case "channel":
		if len(parts) != 3 {
			return nil, fmt.Errorf("invalid ref: %s", ref)
		}
		channel := s.channels[parts[1]]
		if channel == nil {
			return nil, fmt.Errorf("unknown channel ref: %s", ref)
		}
		switch parts[2] {
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
		default:
			return nil, fmt.Errorf("unsupported channel field: %s", parts[2])
		}
	case "system":
		if len(parts) != 2 {
			return nil, fmt.Errorf("invalid ref: %s", ref)
		}
		switch parts[1] {
		case "tickSec":
			return float64(now.Unix()), nil
		case "uptimeSec":
			return time.Since(s.startedAt).Seconds(), nil
		default:
			return nil, fmt.Errorf("unsupported system ref: %s", ref)
		}
	default:
		return nil, fmt.Errorf("unsupported ref: %s", ref)
	}
}

func (r *formulaRuntime) evalOp(s *simulator, expr formulaExpr, now time.Time) (any, error) {
	args := make([]any, 0, len(expr.Args))
	for _, argExpr := range expr.Args {
		value, err := r.evalExpr(s, argExpr, now)
		if err != nil {
			return nil, err
		}
		args = append(args, value)
	}
	switch expr.Op {
	case "+":
		return asFloat(args[0]) + asFloat(args[1]), nil
	case "-":
		return asFloat(args[0]) - asFloat(args[1]), nil
	case "*":
		return asFloat(args[0]) * asFloat(args[1]), nil
	case "/":
		if asFloat(args[1]) == 0 {
			return 0.0, nil
		}
		return asFloat(args[0]) / asFloat(args[1]), nil
	case "min":
		return minArgs(args), nil
	case "max":
		return maxArgs(args), nil
	case "clamp":
		return clampFloat(asFloat(args[0]), asFloat(args[1]), asFloat(args[2])), nil
	case "if":
		if asBool(args[0]) {
			return args[1], nil
		}
		return args[2], nil
	case ">":
		return asFloat(args[0]) > asFloat(args[1]), nil
	case "<":
		return asFloat(args[0]) < asFloat(args[1]), nil
	case ">=":
		return asFloat(args[0]) >= asFloat(args[1]), nil
	case "<=":
		return asFloat(args[0]) <= asFloat(args[1]), nil
	case "==":
		return compareEq(args[0], args[1]), nil
	case "!=":
		return !compareEq(args[0], args[1]), nil
	default:
		return nil, fmt.Errorf("unsupported op: %s", expr.Op)
	}
}

func minArgs(values []any) float64 {
	current := asFloat(values[0])
	for _, value := range values[1:] {
		current = math.Min(current, asFloat(value))
	}
	return current
}

func maxArgs(values []any) float64 {
	current := asFloat(values[0])
	for _, value := range values[1:] {
		current = math.Max(current, asFloat(value))
	}
	return current
}

func clampFloat(value float64, minValue float64, maxValue float64) float64 {
	if value < minValue {
		return minValue
	}
	if value > maxValue {
		return maxValue
	}
	return value
}

func compareEq(left any, right any) bool {
	switch leftValue := left.(type) {
	case string:
		return leftValue == asString(right)
	case bool:
		return leftValue == asBool(right)
	default:
		return math.Abs(asFloat(left)-asFloat(right)) < 0.000001
	}
}

func asFloat(value any) float64 {
	switch typed := value.(type) {
	case nil:
		return 0
	case float64:
		return typed
	case float32:
		return float64(typed)
	case int:
		return float64(typed)
	case int32:
		return float64(typed)
	case int64:
		return float64(typed)
	case uint:
		return float64(typed)
	case uint32:
		return float64(typed)
	case uint64:
		return float64(typed)
	case bool:
		if typed {
			return 1
		}
		return 0
	case string:
		if typed == "" {
			return 0
		}
		return parseFloat(typed, 0)
	default:
		return 0
	}
}

func asBool(value any) bool {
	switch typed := value.(type) {
	case bool:
		return typed
	case string:
		return typed == "true" || typed == "on" || typed == "1" || typed == "alarm"
	default:
		return asFloat(value) != 0
	}
}

func asString(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case bool:
		if typed {
			return "true"
		}
		return "false"
	default:
		return fmt.Sprintf("%v", value)
	}
}
