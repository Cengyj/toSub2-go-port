package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"tosub2-go-port/internal/browser"
)

func TestChromiumEmbeddedConsoleInteractions(t *testing.T) {
	if os.Getenv("TOSUB2_BROWSER_TEST") != "1" {
		t.Skip("set TOSUB2_BROWSER_TEST=1 for real Chromium embedded React acceptance")
	}
	chrome := os.Getenv("CHROME_PATH")
	if chrome == "" {
		chrome = browser.DefaultChrome()
	}
	if chrome == "" {
		t.Skip("Chromium not installed; set CHROME_PATH")
	}
	process := startConsoleProcess(t, t.TempDir())
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	pool := browser.NewManager(chrome, 1)
	defer pool.Close()
	session, err := pool.Start(ctx, "", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err = session.Navigate(ctx, process.base+"/"); err != nil {
		t.Fatal(err)
	}
	evaluate := func(expression string) any {
		t.Helper()
		v, err := session.Evaluate(ctx, expression)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	wait := func(expression string) {
		t.Helper()
		deadline := time.Now().Add(12 * time.Second)
		for time.Now().Before(deadline) {
			if v, err := session.Evaluate(ctx, expression); err == nil && v == true {
				return
			}
			time.Sleep(40 * time.Millisecond)
		}
		body, _ := session.Evaluate(ctx, "document.body.innerText.slice(0,3500)")
		requests, _ := session.Evaluate(ctx, "JSON.stringify(window.__fixtureRequests)")
		serverLog, _ := os.ReadFile(process.logPath)
		t.Fatalf("React state timeout: %s; page=%v; requests=%v; server=%s", expression, body, requests, serverLog)
	}
	quote := func(text string) string { b, _ := json.Marshal(text); return string(b) }
	setValue := func(selector, value string) {
		t.Helper()
		evaluate(`(()=>{const e=document.querySelector(` + quote(selector) + `);if(!e)throw new Error('fixture input missing');e.focus();e.select();return true})()`)
		if err := session.InsertText(ctx, value); err != nil {
			t.Fatal(err)
		}
		evaluate(`new Promise(resolve=>requestAnimationFrame(()=>requestAnimationFrame(()=>resolve(true))))`)
	}
	clickText := func(label string) {
		t.Helper()
		evaluate(`(()=>{const e=[...document.querySelectorAll('button')].find(x=>x.textContent.trim()===` + quote(label) + `);if(!e||e.disabled)throw new Error('fixture button unavailable');e.click();return true})()`)
	}
	wait(`!!document.querySelector('.add-form button[type=submit]')&&!document.querySelector('.add-form button[type=submit]').disabled`)
	evaluate(`window.__fixtureRequests=[];const originalFetch=window.fetch;window.fetch=async(...args)=>{const record=[String(args[0]),args[1]?.method||'GET','pending'];window.__fixtureRequests.push(record);try{const response=await originalFetch(...args);record[2]=response.status;return response}catch(error){record[2]='failed';throw error}};window.__fixtureErrors=[];window.addEventListener('error',()=>window.__fixtureErrors.push('error'));window.addEventListener('unhandledrejection',()=>window.__fixtureErrors.push('rejection'));window.confirm=()=>true;true`)
	setValue(`input[aria-label="邮箱地址"]`, "single-ui@smoke.invalid")
	evaluate(`document.querySelector('.add-form').requestSubmit();true`)
	wait(`document.body.innerText.includes('共 1 条任务')`)
	clickText("批量添加")
	wait(`!!document.querySelector('#batch-input')`)
	lines := make([]string, 24)
	for i := range lines {
		lines[i] = fmt.Sprintf("batch-ui-%02d@smoke.invalid", i)
	}
	setValue("#batch-input", strings.Join(lines, "\n"))
	wait(`document.querySelector('.batch-dialog button[type=submit]').textContent.includes('24')`)
	evaluate(`document.querySelector('.batch-dialog').requestSubmit();true`)
	wait(`!document.querySelector('#batch-input')&&document.body.innerText.includes('共 25 条任务')&&document.querySelectorAll('.job-row').length===20`)
	clickText("本页全选")
	wait(`document.body.innerText.includes('跨页已选 20 条')`)
	evaluate(`document.querySelector('button[title="下一页"]').click();true`)
	wait(`document.querySelectorAll('.job-row').length===5`)
	clickText("本页全选")
	wait(`document.body.innerText.includes('跨页已选 25 条')`)
	clickText("筛选账号")
	wait(`!!document.querySelector('#filter-input')`)
	setValue("#filter-input", "single-ui@smoke.invalid")
	evaluate(`document.querySelector('.filter-dialog').requestSubmit();true`)
	wait(`document.querySelectorAll('.job-row').length===1&&document.body.innerText.includes('匹配 1 条，共 25 条任务')`)
	evaluate(`document.querySelector('button[title="查看日志"]').click();true`)
	wait(`!!document.querySelector('.log-panel pre')&&!document.querySelector('.log-panel pre').textContent.includes('正在读取')`)
	clickText("清除筛选")
	// Changing the email filter deliberately clears the selection in the
	// legacy React console. Re-select across pages before the delete action.
	wait(`document.querySelectorAll('.job-row').length===20&&document.body.innerText.includes('跨页已选 0 条')`)
	clickText("本页全选")
	wait(`document.body.innerText.includes('跨页已选 20 条')`)
	evaluate(`document.querySelector('button[title="下一页"]').click();true`)
	wait(`document.querySelectorAll('.job-row').length===5`)
	clickText("本页全选")
	wait(`document.body.innerText.includes('跨页已选 25 条')`)
	clickText("批量删除")
	wait(`document.querySelectorAll('.job-row').length===0&&document.body.innerText.includes('添加邮箱后开始第一条任务')`)
	if v := evaluate(`window.__fixtureErrors.length`); v != float64(0) {
		t.Fatalf("browser JavaScript errors: %v", v)
	}
	process.stop(t)
}
