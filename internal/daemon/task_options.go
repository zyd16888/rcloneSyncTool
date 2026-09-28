package daemon

import (
	"context"
	"strings"

	"115togd/internal/store"
)

type taskOptionsKey struct{}

func withTaskOptions(ctx context.Context, rule store.Rule) context.Context {
	args, _ := ParseRcloneArgs(rule.RcloneExtraArgs)
	return context.WithValue(ctx, taskOptionsKey{}, SanitizeRcloneFilterArgs(SanitizeRcloneArgs(args).Args).Args)
}
func taskOptions(ctx context.Context) []string {
	args, _ := ctx.Value(taskOptionsKey{}).([]string)
	return args
}
func applyDriveOptions(ctx context.Context, config map[string]string) {
	args := taskOptions(ctx)
	for i := 0; i < len(args); i++ {
		key, value, hasValue := strings.Cut(args[i], "=")
		if !strings.HasPrefix(key, "--drive-") {
			continue
		}
		key = strings.ReplaceAll(strings.TrimPrefix(key, "--drive-"), "-", "_")
		if !hasValue && i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
			i++
			value = args[i]
			hasValue = true
		}
		if hasValue {
			config[key] = value
		}
	}
}
