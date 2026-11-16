package main

import (
	"fmt"
	"path"
	"strings"

	"gopkg.in/yaml.v3"
)

// pluginConfig is the YAML under plugins.configs.key-account-bind.
//
//	bindings:
//	  - key: "sk-tenant-a"                # must also exist in native api-keys
//	    allow: ["codex-a*.json", "claude-main*"]   # glob on auth file / auth ID
//	  - key: "sk-tenant-b"
//	    allow: ["codex-b*"]
//	unbound: passthrough   # passthrough | deny  (default passthrough)
type pluginConfig struct {
	Bindings bindingList `yaml:"bindings"`
	Unbound  string      `yaml:"unbound"`
	Strategy string      `yaml:"strategy"`
}

// bindingList accepts either compact string entries or full object entries:
//
//	bindings:
//	  - "sk-tenant-a=team-*.json,vip-*"          # compact: key=glob1,glob2
//	  - key: "sk-tenant-b"                        # verbose object form
//	    allow: ["codex-b*"]
//
// The compact form keeps the whole config editable from the CPAMC plugin
// panel, which only supports string arrays natively.
type bindingList []bindingRule

func (l *bindingList) UnmarshalYAML(node *yaml.Node) error {
	*l = nil
	for i, item := range node.Content {
		var rule bindingRule
		switch item.Tag {
		case "!!str":
			parts := strings.SplitN(item.Value, "=", 2)
			if len(parts) != 2 {
				return fmt.Errorf("bindings[%d]: compact entry must be \"key=glob1,glob2\"", i)
			}
			rule.Key = strings.TrimSpace(parts[0])
			for _, a := range strings.Split(parts[1], ",") {
				if a = strings.TrimSpace(a); a != "" {
					rule.Allow = append(rule.Allow, a)
				}
			}
		case "!!map":
			if err := item.Decode(&rule); err != nil {
				return fmt.Errorf("bindings[%d]: %w", i, err)
			}
		default:
			return fmt.Errorf("bindings[%d]: must be a string or a mapping", i)
		}
		*l = append(*l, rule)
	}
	return nil
}

type bindingRule struct {
	Key   string   `yaml:"key"`
	Allow []string `yaml:"allow"`
}

// policy is the immutable, compiled form of pluginConfig.
type policy struct {
	byScope            map[string]*binding
	unboundPassthrough bool
	// strategy is applied inside the filtered/allowed candidate set. The current
	// CPA plugin ABI cannot pass a filtered set back to the native selector, so
	// the plugin mirrors the built-in strategy locally.
	strategy string
}

type binding struct {
	patterns []string // normalized (lowercase) glob patterns
}

// matches reports whether an auth ID (lowercased) is allowed by the binding.
func (b *binding) matches(id string) bool {
	if b == nil {
		return false
	}
	id = strings.ToLower(id)
	for _, p := range b.patterns {
		if ok, err := path.Match(p, id); err == nil && ok {
			return true
		}
	}
	return false
}

func parsePolicy(raw []byte) (*policy, error) {
	var cfg pluginConfig
	if len(raw) > 0 {
		if err := yaml.Unmarshal(raw, &cfg); err != nil {
			return nil, fmt.Errorf("parse config: %w", err)
		}
	}
	p := &policy{byScope: make(map[string]*binding, len(cfg.Bindings))}
	switch strings.ToLower(strings.TrimSpace(cfg.Unbound)) {
	case "passthrough":
		p.unboundPassthrough = true
	case "", "deny":
		p.unboundPassthrough = false
	default:
		return nil, fmt.Errorf("invalid unbound policy %q (want passthrough or deny)", cfg.Unbound)
	}
	strategy, errStrategy := normalizeStrategy(cfg.Strategy)
	if errStrategy != nil {
		return nil, errStrategy
	}
	p.strategy = strategy
	for i, rule := range cfg.Bindings {
		key := strings.TrimSpace(rule.Key)
		if key == "" {
			return nil, fmt.Errorf("bindings[%d]: key is required", i)
		}
		patterns := make([]string, 0, len(rule.Allow))
		for _, a := range rule.Allow {
			a = strings.ToLower(strings.TrimSpace(a))
			if a == "" {
				continue
			}
			if _, err := path.Match(a, "x"); err != nil {
				return nil, fmt.Errorf("bindings[%d]: invalid glob %q: %w", i, a, err)
			}
			patterns = append(patterns, a)
		}
		if len(patterns) == 0 {
			return nil, fmt.Errorf("bindings[%d]: at least one allow pattern is required", i)
		}
		scope := callerScopeForKey(key)
		if _, dup := p.byScope[scope]; dup {
			return nil, fmt.Errorf("bindings[%d]: duplicate key", i)
		}
		p.byScope[scope] = &binding{patterns: patterns}
	}
	return p, nil
}

func (p *policy) bindingForScope(scope string) *binding {
	if p == nil || scope == "" {
		return nil
	}
	return p.byScope[scope]
}

// validateConfig is used by tests to assert compile-time config errors.
func validateConfig(raw []byte) error {
	_, err := parsePolicy(raw)
	return err
}

func normalizeStrategy(raw string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "round-robin", "roundrobin", "rr":
		return strategyRoundRobin, nil
	case "weighted-round-robin", "weightedroundrobin", "wrr":
		return strategyWeightedRoundRobin, nil
	case "fill-first", "fillfirst", "ff":
		return strategyFillFirst, nil
	default:
		return "", fmt.Errorf("invalid strategy %q (want round-robin, weighted-round-robin, or fill-first)", raw)
	}
}
