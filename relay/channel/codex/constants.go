package codex

import (
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/samber/lo"
)

var baseModelList = []string{
	"gpt-5.4",
	"gpt-5.3-codex", "gpt-5.3-codex-spark",
	"gpt-5.2-codex",
	"gpt-5.1-codex",
}

var ModelList = withCompactModelSuffix(baseModelList)

const ChannelName = "codex"

func withCompactModelSuffix(models []string) []string {
	out := make([]string, 0, len(models)*2)
	out = append(out, models...)
	out = append(out, lo.Map(models, func(model string, _ int) string {
		return ratio_setting.WithCompactModelSuffix(model)
	})...)
	return lo.Uniq(out)
}
