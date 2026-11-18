package docs

import (
	"strings"
	"testing"
)

func TestSetting(t *testing.T) {
	if text, err := Setting("NOTIFY_KEEP_SENT"); err != nil || text != "Delivered messages are deleted from the delivery log after this." {
		t.Errorf("NOTIFY_KEEP_SENT: %q %v", text, err)
	}

	// Markdown is dropped: the text goes into a tooltip.
	if text, err := Setting("ADMIN_PASSWORD"); err != nil || strings.ContainsAny(text, "`[]") {
		t.Errorf("ADMIN_PASSWORD: %q %v", text, err)
	}

	if _, err := Setting("NOT_A_SETTING"); err == nil {
		t.Error("an unknown name has a description")
	}
}

func TestField(t *testing.T) {
	if text, err := Field("Notifications", "Chat ID"); err != nil || !strings.HasPrefix(text, "The chat the bot writes to") {
		t.Errorf("Chat ID: %q %v", text, err)
	}

	// Fields are looked up in their own section; headers are not fields.
	for _, missing := range [][2]string{{"Projects", "Chat ID"}, {"Notifications", "Field"}, {"Notifications", ""}} {
		if _, err := Field(missing[0], missing[1]); err == nil {
			t.Errorf("%v has a description", missing)
		}
	}
}
