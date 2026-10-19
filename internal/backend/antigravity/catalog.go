package antigravity

import (
	"context"
	"strings"

	"github.com/tyro668/avora/internal/backend"
)

func (a *Adapter) Catalog(ctx context.Context) ([]backend.ModelInfo, error) {
	text, err := a.modelOutput(ctx)
	if err != nil {
		return nil, err
	}
	return parseCatalog(text)
}

// agy publishes pinned effort variants as model IDs. Offer only sibling
// variants present in the live catalog, with matching display labels. A lone
// suffix (e.g. gpt-oss-120b-medium) is not evidence of adjustability.
func parseCatalog(text string) ([]backend.ModelInfo, error) {
	ids, err := parseModels(text)
	if err != nil {
		return nil, err
	}
	labels := map[string]string{}
	for _, line := range strings.Split(text, "\n") {
		id, label, ok := strings.Cut(strings.TrimSpace(line), "\t")
		if ok {
			labels[id] = label
		}
	}
	levels := []struct{ value, label string }{{"none", "None"}, {"minimal", "Minimal"}, {"low", "Low"}, {"medium", "Medium"}, {"high", "High"}, {"xhigh", "Extra High"}, {"max", "Max"}, {"ultra", "Ultra"}}
	groups := map[string][]backend.EffortOption{}
	families, efforts := map[string]string{}, map[string]string{}
	for _, id := range ids {
		for _, level := range levels {
			suffix := "-" + level.value
			labelSuffix := " (" + level.label + ")"
			if !strings.HasSuffix(id, suffix) || !strings.HasSuffix(labels[id], labelSuffix) {
				continue
			}
			family := strings.TrimSuffix(id, suffix) + "\t" + strings.TrimSuffix(labels[id], labelSuffix)
			families[id], efforts[id] = family, level.value
			groups[family] = append(groups[family], backend.EffortOption{Value: level.value, Model: id})
		}
	}
	models := make([]backend.ModelInfo, 0, len(ids))
	for _, id := range ids {
		info := backend.ModelInfo{ID: id, ReasoningKnown: true, ReasoningEfforts: []backend.EffortOption{}}
		if siblings := groups[families[id]]; len(siblings) > 1 {
			info.DefaultReasoningEffort = efforts[id]
			// Stable presentation order, independent of the CLI's model order.
			for _, level := range levels {
				for _, option := range siblings {
					if option.Value == level.value {
						info.ReasoningEfforts = append(info.ReasoningEfforts, option)
					}
				}
			}
		}
		models = append(models, info)
	}
	return models, nil
}
