package config

import "testing"

func TestNormalizeProviders(t *testing.T) {
	c := Defaults()
	c.Providers = map[string]ProviderProfile{
		"glm":    {URL: "https://open.bigmodel.cn/api/paas/v4/", APIKey: "$GLM_KEY", Model: "glm-4.6"},
		"broken": {URL: ""},
		"keep":   {URL: "https://api.deepseek.com"},
	}
	c.ActiveProvider = "broken"

	c.normalize()

	if _, ok := c.Providers["broken"]; ok {
		t.Fatal("empty-url profile not dropped")
	}
	if got := c.Providers["glm"].URL; got != "https://open.bigmodel.cn/api/paas/v4" {
		t.Fatalf("url not trimmed: %q", got)
	}
	if _, ok := c.Providers["keep"]; !ok {
		t.Fatal("valid profile dropped")
	}
	if c.ActiveProvider != "" {
		t.Fatalf("active provider must reset when its profile is dropped: %q", c.ActiveProvider)
	}

	// a live profile keeps the active provider
	c.ActiveProvider = "glm"
	c.normalize()
	if c.ActiveProvider != "glm" {
		t.Fatalf("valid active provider reset: %q", c.ActiveProvider)
	}

	// ResolvedKey: $ENV reference vs plaintext
	t.Setenv("GLM_KEY", "sk-from-env")
	if got := c.Providers["glm"].ResolvedKey(); got != "sk-from-env" {
		t.Fatalf("env reference not expanded: %q", got)
	}
	if got := c.Providers["keep"].ResolvedKey(); got != "" {
		t.Fatalf("empty key resolved to %q", got)
	}
}
