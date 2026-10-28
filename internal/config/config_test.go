package config

import "testing"

func TestNormalizeAllowedHosts(t *testing.T) {
	got, err := NormalizeAllowedHosts([]string{" *.HF.co. ", "HF.co"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != "*.hf.co" || got[1] != "hf.co" {
		t.Fatalf("normalized hosts = %#v", got)
	}
	for _, value := range []string{"*", "*.co", "foo/bar"} {
		if _, err := NormalizeAllowedHosts([]string{value}); err == nil {
			t.Errorf("NormalizeAllowedHosts(%q) accepted invalid host", value)
		}
	}
}

func TestValidateEnvironmentOverridesRejectsInvalidValues(t *testing.T) {
	t.Setenv("HFPROXY_HF_TIMEOUT", "30")
	if err := validateEnvironmentOverrides(); err == nil {
		t.Fatal("invalid duration was accepted")
	}
	t.Setenv("HFPROXY_HF_TIMEOUT", "")
	t.Setenv("HFPROXY_CACHE_MAX_FILE_SIZE", "10GiB")
	if err := validateEnvironmentOverrides(); err == nil {
		t.Fatal("invalid cache size was accepted")
	}
}
