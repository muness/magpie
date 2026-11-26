package settingsapi

import (
	"testing"

	"github.com/yetone/magpie/internal/settings"
)

func TestCodexCheckpointModelOptIn(t *testing.T) {
	home(t)
	if settings.Load().CodexCheckpointModel != "" {
		t.Fatal("checkpoint recovery enabled by default")
	}
	if err := settings.Save(settings.Settings{CodexCheckpointModel: " gpt-6-luna "}); err != nil {
		t.Fatal(err)
	}
	if settings.Load().CodexCheckpointModel != "gpt-6-luna" {
		t.Fatal("recovery model not saved")
	}
	if _, _, err := Apply([]byte(`{"theme":"dark"}`), false); err != nil {
		t.Fatal(err)
	}
	if settings.Load().CodexCheckpointModel != "gpt-6-luna" {
		t.Fatal("unrelated settings save lost the opt-in")
	}
	for _, invalid := range []string{"codex/gpt-6-luna", "gpt 6", "gpt\nother"} {
		if err := settings.Save(settings.Settings{CodexCheckpointModel: invalid}); err == nil {
			t.Fatalf("accepted %s", invalid)
		}
		if settings.Load().CodexCheckpointModel != "gpt-6-luna" {
			t.Fatal("failed save changed recovery model")
		}
	}
	s := settings.Load()
	s.CodexCheckpointModel = ""
	if err := settings.Save(s); err != nil {
		t.Fatal(err)
	}
	if settings.Load().CodexCheckpointModel != "" {
		t.Fatal("could not disable recovery")
	}
}
