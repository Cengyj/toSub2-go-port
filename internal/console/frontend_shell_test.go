package console

import "testing"

// Render the actual App and then its actual AppShell in the existing synthetic
// React boundary. Keeping their hook slots adjacent preserves both components'
// event state while exercising production navigation handlers. CSS geometry and
// native focus behavior still require the separate browser verification.
const frontendWorkspaceQueries = `
function renderWorkspace() {
  const app = render();
  return RepairUI.AppShell(find(app, "AppShell").props);
}
function workspaceNavigation(tree, label) {
  return findMatching(tree, node => node.type === "button" && node.props.title === label && (node.props.className || "").split(" ").includes("nav-link"));
}
function assertWorkspace(tree, page, label) {
  const main = find(tree, "main");
  assert(main && main.props.className === (page === "accounts" ? "workspace workspace-accounts" : "workspace"), "workspace width scope disagrees with the active page: " + page);
  assert(main.props.id === "main-content" && main.props.tabIndex === -1, "page width change removed the keyboard focus destination");
  const skip = findMatching(tree, node => node.type === "a" && node.props.className === "skip-link");
  assert(skip && skip.props.href === "#main-content", "workspace lost the skip-navigation target");
  assert(workspaceNavigation(tree, label).props["aria-current"] === "page", "workspace page and active navigation disagree");
  const accounts = findMatching(main, node => node.type === "section" && node.props.className === "accounts-section");
  assert(accounts && find(accounts, "table") && accounts.props.hidden === (page !== "accounts"), "retained account table visibility does not follow actual page navigation");
  assert(Boolean(find(main, "ConfigurationPage")) === (page === "settings"), "configuration content escaped its original page scope");
  assert(Boolean(find(main, "MonitorPage")) === (page === "monitor"), "monitor content escaped its original page scope");
}
`

func TestFrontendShellWorkspaceWidthTracksActualPageNavigation(t *testing.T) {
	for _, tc := range []struct{ page, label string }{
		{"accounts", "账号管理"}, {"settings", "服务配置"}, {"monitor", "号池监控"},
	} {
		t.Run(tc.page, func(t *testing.T) {
			vm := frontendAutoRepairComponentVM(t, "shell")
			for name, value := range map[string]string{"initialPage": tc.page, "initialLabel": tc.label} {
				if err := vm.Set(name, value); err != nil {
					t.Fatal(err)
				}
			}
			frontendPollingRun(t, vm, frontendWorkspaceQueries)
			frontendPollingRun(t, vm, `
location.hash = "#" + initialPage;
let tree = renderWorkspace();
assertWorkspace(tree, initialPage, initialLabel);
for (const [page, label] of [["accounts","账号管理"],["settings","服务配置"],["monitor","号池监控"],["accounts","账号管理"]]) {
  workspaceNavigation(tree, label).props.onClick();
  tree = renderWorkspace();
  assertWorkspace(tree, page, label);
}
const collapse = findMatching(tree, node => node.type === "button" && node.props["aria-label"] === "收起侧栏");
collapse.props.onClick();
tree = renderWorkspace();
assert(tree.props.className.split(" ").includes("sidebar-collapsed"), "actual sidebar collapse handler did not update its layout state");
assertWorkspace(tree, "accounts", "账号管理");
workspaceNavigation(tree, "服务配置").props.onClick();
tree = renderWorkspace();
assertWorkspace(tree, "settings", "服务配置");
assert(tree.props.className.split(" ").includes("sidebar-collapsed"), "page navigation reset the user's current sidebar state");
assert(requests.length === 0, "layout navigation handlers directly submitted an account or settings request");
`)
		})
	}
}

func TestFrontendShellWorkspaceTracksAcceptedNavigationAfterDraftGuard(t *testing.T) {
	vm := frontendAutoRepairComponentVM(t, "shell")
	frontendPollingRun(t, vm, frontendWorkspaceQueries)
	frontendPollingRun(t, vm, `
location.hash = "#settings";
let tree = renderWorkspace();
const stored = [...storage.entries()];
find(tree, "ConfigurationPage").props.onSectionChange("sub2api");
tree = renderWorkspace();
find(tree, "Sub2ApiSettingsDialog").props.setSub2apiSettingsDraft(value => ({...value, baseUrl:"https://synthetic-unsaved.invalid"}));
tree = renderWorkspace();
workspaceNavigation(tree, "账号管理").props.onClick();
tree = renderWorkspace();
assertWorkspace(tree, "settings", "服务配置");
assert(findButton(tree, "继续编辑") && findButton(tree, "放弃更改并离开"), "layout navigation bypassed the existing unsaved-draft guard");
findButton(tree, "继续编辑").props.onClick();
tree = renderWorkspace();
assertWorkspace(tree, "settings", "服务配置");
assert(find(tree, "Sub2ApiSettingsDialog").props.sub2apiSettingsDraft.baseUrl === "https://synthetic-unsaved.invalid", "canceling navigation lost the unsaved draft");
workspaceNavigation(tree, "账号管理").props.onClick();
tree = renderWorkspace();
findButton(tree, "放弃更改并离开").props.onClick();
tree = renderWorkspace();
assertWorkspace(tree, "accounts", "账号管理");
assert(!find(tree, "Sub2ApiSettingsDialog") && !findButton(tree, "放弃更改并离开"), "accepted navigation left a stale editor or confirmation open");
assert(JSON.stringify([...storage.entries()]) === JSON.stringify(stored) && requests.length === 0, "width or guarded navigation persisted an unsaved configuration");
`)
}
