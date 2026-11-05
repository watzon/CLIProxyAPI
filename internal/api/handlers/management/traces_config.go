package management

import (
	"context"
	"fmt"
	"os"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/traces"
	"gopkg.in/yaml.v3"
)

// SetTracesConfigValue replaces the value at traces.<path...> in config.yaml
// through the same v8 save path as PUT /v8/management/config/*path (layout
// normalization, validation, comment-preserving write, async reload) and
// returns the saved configuration.
func (h *Handler) SetTracesConfigValue(ctx context.Context, path []string, value any) (*config.Config, error) {
	if h == nil || len(path) == 0 {
		return nil, fmt.Errorf("traces config path is empty")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	raw, err := os.ReadFile(h.configFilePath)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	data, _, err := config.NormalizeConfigLayout(raw, true)
	if err != nil {
		return nil, fmt.Errorf("normalize config: %w", err)
	}
	var doc yaml.Node
	if err = yaml.Unmarshal(data, &doc); err != nil || len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("config is not a YAML mapping")
	}
	var node yaml.Node
	if err = node.Encode(value); err != nil {
		return nil, fmt.Errorf("encode traces value: %w", err)
	}
	full := append([]string{"traces"}, path...)
	dst := doc.Content[0]
	for _, part := range full[:len(full)-1] {
		next := configV8Node(dst, []string{part})
		if next == nil || next.Kind != yaml.MappingNode {
			created := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
			if next != nil {
				*next = *created
				created = next
			} else {
				dst.Content = append(dst.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: part}, created)
			}
			next = created
		}
		dst = next
	}
	last := full[len(full)-1]
	if existing := configV8Node(dst, []string{last}); existing != nil {
		node.HeadComment, node.LineComment, node.FootComment = existing.HeadComment, existing.LineComment, existing.FootComment
		*existing = node
	} else {
		dst.Content = append(dst.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: last}, &node)
	}
	config.NormalizeCollectionNodeStyles(doc.Content[0])
	if data, err = yaml.Marshal(&doc); err != nil {
		return nil, fmt.Errorf("encode config: %w", err)
	}
	next, err := config.ParseConfigBytes(data)
	if err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}
	if err = config.ValidateV8Config(data); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}
	if data, _, err = config.NormalizeConfigLayout(data, true); err != nil {
		return nil, fmt.Errorf("normalize config: %w", err)
	}
	if err = WriteConfig(h.configFilePath, data); err != nil {
		return nil, fmt.Errorf("write config: %w", err)
	}
	next.Home = h.cfg.Home
	h.cfg = next
	snapshot := h.reloadSnapshotConfigLocked()
	h.reloadConfigAfterManagementSaveAsync(ctx, snapshot)
	return next.CloneForRuntime(), nil
}

// tracesConfigView prepares a JSON config read. traces.capture-by-key maps raw
// client keys, so its keys are shown as key labels (the key_label rule of the
// traces API). A read of the traces block also fills unset keys with defaults,
// so the block answers even when config.yaml does not have it.
func tracesConfigView(root *yaml.Node, parts []string) {
	if root == nil || root.Kind != yaml.MappingNode {
		return
	}
	if len(parts) > 0 && parts[0] == "traces" {
		var defaults yaml.Node
		if err := yaml.Unmarshal([]byte(traces.DefaultConfigYAML), &defaults); err == nil && len(defaults.Content) > 0 {
			block := configV8Node(root, []string{"traces"})
			if block == nil || block.Kind != yaml.MappingNode {
				block = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
				root.Content = append(root.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "traces"}, block)
			}
			fillMissingKeys(block, defaults.Content[0])
		}
	}
	if byKey := configV8Node(root, []string{"traces", "capture-by-key"}); byKey != nil && byKey.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(byKey.Content); i += 2 {
			byKey.Content[i].Value = traces.KeyLabel(byKey.Content[i].Value)
		}
	}
}

// fillMissingKeys copies keys of defaults that dst lacks, recursing into maps.
func fillMissingKeys(dst, defaults *yaml.Node) {
	for i := 0; i+1 < len(defaults.Content); i += 2 {
		key, value := defaults.Content[i], defaults.Content[i+1]
		existing := configV8Node(dst, []string{key.Value})
		switch {
		case existing == nil:
			dst.Content = append(dst.Content, key, value)
		case existing.Kind == yaml.MappingNode && value.Kind == yaml.MappingNode:
			fillMissingKeys(existing, value)
		}
	}
}

// preserveTracesCaptureKeys turns key labels sent back by a client (after a
// JSON read showed them) into the raw client keys stored before the write.
func preserveTracesCaptureKeys(root, before *yaml.Node) {
	next := configV8Node(root, []string{"traces", "capture-by-key"})
	prev := configV8Node(before, []string{"traces", "capture-by-key"})
	if next == nil || prev == nil || next.Kind != yaml.MappingNode || prev.Kind != yaml.MappingNode {
		return
	}
	raw := map[string]string{}
	for i := 0; i+1 < len(prev.Content); i += 2 {
		raw[traces.KeyLabel(prev.Content[i].Value)] = prev.Content[i].Value
	}
	for i := 0; i+1 < len(next.Content); i += 2 {
		if key, ok := raw[next.Content[i].Value]; ok {
			next.Content[i].Value = key
		}
	}
	// A PATCH merges the label next to the stored raw key; the later entry
	// (the value the client sent) wins.
	last := map[string]int{}
	for i := 0; i+1 < len(next.Content); i += 2 {
		last[next.Content[i].Value] = i
	}
	kept := next.Content[:0]
	for i := 0; i+1 < len(next.Content); i += 2 {
		if last[next.Content[i].Value] == i {
			kept = append(kept, next.Content[i], next.Content[i+1])
		}
	}
	next.Content = kept
}
