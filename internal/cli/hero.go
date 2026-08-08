package cli

func heroSummaryStyle(env *Env, s string) string {
	if env == nil || !env.Color {
		return s
	}
	return ansiBold + ansiYellow + s + ansiReset
}
