package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadDotEnv(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, ".env")
	content := "# comment\n\nBBT_T_A=uno\nexport BBT_T_B=\"dos con espacios\"\nBBT_T_C='postgres://u:p@h/db?sslmode=disable'\nBBT_T_D=4 # inline\nBBT_T_SET=from-file\n"
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BBT_T_SET", "from-env")
	for _, k := range []string{"BBT_T_A", "BBT_T_B", "BBT_T_C", "BBT_T_D"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	n, err := LoadDotEnv(p)
	if err != nil || n != 4 {
		t.Fatalf("loaded=%d err=%v", n, err)
	}
	want := map[string]string{"BBT_T_A": "uno", "BBT_T_B": "dos con espacios",
		"BBT_T_C": "postgres://u:p@h/db?sslmode=disable", "BBT_T_D": "4", "BBT_T_SET": "from-env"}
	for k, v := range want {
		if got := os.Getenv(k); got != v {
			t.Errorf("%s=%q want %q", k, got, v)
		}
	}
	if n, err := LoadDotEnv(filepath.Join(dir, "missing")); n != 0 || err != nil {
		t.Fatal("a missing file is not an error")
	}
	if err := os.WriteFile(p, []byte("not a pair\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadDotEnv(p); err == nil {
		t.Fatal("malformed line must fail")
	}
}
