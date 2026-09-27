package challenge

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"tosub2-go-port/internal/browser"
)

func TestInteractionPromptExplainsHeadlessRestartAndDesktop(t *testing.T) {
	for _, err := range []error{browser.ErrInteractiveBrowserRequired, fmt.Errorf("private-CDP-detail: %w", browser.ErrInteractiveBrowserRequired)} {
		prompt := interactionPrompt(err)
		for _, detail := range []string{"无头会话", "无法在运行中切换", "TOSUB2_BROWSER_HEADLESS=0", "重启服务", "新的浏览器会话", "Docker", "桌面/显示服务", "仅修改该环境变量不够", "macOS", "原生服务", "账号本人"} {
			if !strings.Contains(prompt, detail) {
				t.Errorf("headless prompt omits %q: %s", detail, prompt)
			}
		}
		if strings.Contains(prompt, "private-CDP-detail") {
			t.Fatal("headless prompt leaked the underlying browser error")
		}
	}
}

func TestInteractionPromptDoesNotMisclassifyVisibleWindowFailure(t *testing.T) {
	prompt := interactionPrompt(errors.New("private-CDP-detail access_token=synthetic"))
	if !strings.Contains(prompt, "窗口无法显示") || !strings.Contains(prompt, "图形桌面") {
		t.Fatal("window failure did not explain the desktop requirement")
	}
	for _, forbidden := range []string{"无头会话", "TOSUB2_BROWSER_HEADLESS=0", "private-CDP-detail", "synthetic", "access_token"} {
		if strings.Contains(prompt, forbidden) {
			t.Fatalf("window failure was misclassified or exposed raw error text: %q", forbidden)
		}
	}
}

func TestInteractionPromptForPresentedWindowPreservesManualVerification(t *testing.T) {
	prompt := interactionPrompt(nil)
	if !strings.Contains(prompt, "账号本人") || !strings.Contains(prompt, "当前 Chromium 窗口") || !strings.Contains(prompt, "等待网站返回结果") {
		t.Fatal("presented-window guidance must retain account-owner manual interaction")
	}
	if strings.Contains(prompt, "重启") {
		t.Fatal("a presented visible window should not ask the user to restart")
	}
}
