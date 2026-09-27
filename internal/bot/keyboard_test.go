package bot

import (
	"strings"
	"testing"

	"Max-hack/internal/maxapi"
)

func TestMessageWithRestartAlwaysAddsOneRestartButton(t *testing.T) {
	tests := []struct {
		name string
		rows [][]maxapi.Button
	}{
		{name: "empty"},
		{name: "start", rows: startKeyboardRows()},
		{name: "already present", rows: restartKeyboardRows()},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := messageWithRestart("Сообщение", tt.rows)
			buttons := inlineKeyboardButtons(body)
			if got := callbackCount(buttons, payloadRestart); got != 1 {
				t.Fatalf("restart button count = %d, want 1; buttons=%#v", got, buttons)
			}
			if !hasButtonLabel(buttons, payloadRestart, "🔄 Начать заново") {
				t.Fatalf("restart button has unexpected label: %#v", buttons)
			}
		})
	}
}

func TestRepeatedQualificationSelectionKeepsCurrentActions(t *testing.T) {
	cat := loadTestCatalog(t)
	h := NewHandler(newFakeMessenger(), cat, nil, nil, AnalysisConfig{})
	program, ok := cat.Get("09.02.07")
	if !ok || len(program.Qualifications) == 0 {
		t.Fatal("test program or qualification not found")
	}
	h.sessions.set(1, session{
		Step: stepAwaitQual, Generation: 3, RegionCode: "7700000000000", RegionName: "Москва",
		ProgramCode: program.Code,
	})

	_, _, _ = h.selectQualification(1, program.Code, 0)
	_, rows, _ := h.selectQualification(1, program.Code, 0)

	if !hasCallback(rows, payloadRestart) {
		t.Fatalf("stale qualification response has no restart button: %#v", rows)
	}
	if !hasCallbackPrefix(rows, payloadAnalyze) {
		t.Fatalf("stale qualification response lost analyze button: %#v", rows)
	}
}

func TestBeginCallbackIncludesRestartButton(t *testing.T) {
	messenger := newFakeMessenger()
	h := NewHandler(messenger, loadTestCatalog(t), nil, nil, AnalysisConfig{})
	update := maxapi.Update{
		UpdateType: maxapi.UpdateMessageCallback,
		ChatID:     7,
		Callback: &maxapi.Callback{
			CallbackID: "begin",
			Payload:    payloadBegin,
			User:       maxapi.User{UserID: 9},
		},
	}

	h.onCallback(t.Context(), update)
	answer := receive(t, messenger.answers)
	if answer.req.Message == nil {
		t.Fatal("callback answer has no message")
	}
	assertRestartMessage(t, *answer.req.Message)
}

func assertRestartMessage(t *testing.T, body maxapi.NewMessageBody) {
	t.Helper()
	buttons := inlineKeyboardButtons(body)
	if !hasCallback(buttons, payloadRestart) {
		t.Fatalf("message has no restart button: %#v", body)
	}
}

func inlineKeyboardButtons(body maxapi.NewMessageBody) [][]maxapi.Button {
	for _, attachment := range body.Attachments {
		if attachment.Type != "inline_keyboard" {
			continue
		}
		if payload, ok := attachment.Payload.(maxapi.InlineKeyboardPayload); ok {
			return payload.Buttons
		}
	}
	return nil
}

func callbackCount(rows [][]maxapi.Button, payload string) int {
	count := 0
	for _, row := range rows {
		for _, button := range row {
			if button.Type == "callback" && button.Payload == payload {
				count++
			}
		}
	}
	return count
}

func hasCallback(rows [][]maxapi.Button, payload string) bool {
	return callbackCount(rows, payload) > 0
}

func hasCallbackPrefix(rows [][]maxapi.Button, prefix string) bool {
	for _, row := range rows {
		for _, button := range row {
			if button.Type == "callback" && strings.HasPrefix(button.Payload, prefix) {
				return true
			}
		}
	}
	return false
}

func hasButtonLabel(rows [][]maxapi.Button, payload, label string) bool {
	for _, row := range rows {
		for _, button := range row {
			if button.Type == "callback" && button.Payload == payload && button.Text == label {
				return true
			}
		}
	}
	return false
}
