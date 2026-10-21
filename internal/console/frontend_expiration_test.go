package console

import (
	"os"
	"os/exec"
	"testing"
	"time"
)

func TestFrontendExpirationUsesManualThenSubscriptionWithoutTokenExpiry(t *testing.T) {
	vm := frontendBrowserVM(t, "accountExpiration")
	frontendPollingRun(t, vm, `
const now = Date.parse("2026-10-18T09:00:00Z");
const manual = Browser.describeExpiration({manualExpiresAt:"2026-10-18T08:30:00Z",subscriptionExpiresAt:"2026-11-18T08:30:00Z"}, now);
assert(manual.source === "manual" && manual.sourceLabel === "手动" && manual.expired, "past manual expiry was replaced or rolled forward");
assert(manual.remaining === "已到期" && manual.dateTime === "2026-10-18T08:30:00.000Z", "manual expiry lost its actual instant");
const subscription = Browser.describeExpiration({manualExpiresAt:"",subscriptionExpiresAt:"2026-10-20T09:00:00Z"}, now);
assert(subscription.source === "subscription" && subscription.sourceLabel === "套餐" && subscription.remaining === "剩余 2 天", "clearing manual expiry did not fall back to subscription");
for (const job of [{}, {tokenExpiresAt:"2026-10-20T09:00:00Z",expires_at:1792400000}, {expiresAt:"2026-10-20T09:00:00Z",expiresAtSource:"unknown"}, {manualExpiresAt:"invalid"}]) {
  const unknown = Browser.describeExpiration(job, now);
  assert(unknown.source === "unknown" && unknown.label === "未提供到期时间" && !unknown.expired, "untrusted or token expiry became account expiration");
}
assert(Browser.describeExpiration({expiresAt:"2026-10-18T09:00:00Z",expiresAtSource:"manual"}, now).expired, "exact expiry boundary not expired");
assert(Browser.describeExpiration({manualExpiresAt:"2026-10-18T09:00:20Z"}, now).remaining === "不足 1 分钟", "near expiry overstates remaining time");
`)
}

func TestFrontendExpirationTimezoneAndEditorPreserveLocalInput(t *testing.T) {
	vm := frontendBrowserVM(t, "accountExpiration")
	frontendPollingRun(t, vm, `
Intl.DateTimeFormat = function() {return {resolvedOptions() {return {timeZone:"Asia/Shanghai"};}};};
assert(Browser.browserTimeZone() === "Asia/Shanghai", "browser timezone missing");
Intl.DateTimeFormat = function() {throw new Error("synthetic Intl failure");};
assert(Browser.browserTimeZone() === "", "missing timezone silently invented a timezone");
assert(Browser.expirationInputValue("invalid") === "", "invalid persisted date entered editor");
assert(/^2026-10-18T[0-9]{2}:[0-9]{2}$/.test(Browser.expirationInputValue("2026-10-18T09:30:00Z")), "editor value not datetime-local");
`)
}

func TestFrontendExpirationRejectsInvalidPersistedTimestamps(t *testing.T) {
	vm := frontendBrowserVM(t, "accountExpiration")
	frontendPollingRun(t, vm, `
const validSubscription = "2026-07-18T08:00:00Z";
for (const value of [
  null, 1784361600000, "2026-07-18T08:00:00", "2026-07-18",
  "2026-02-30T08:30:00Z", "2025-02-29T08:30:00Z", "1900-02-29T08:30:00Z",
  "2026-04-31T08:30:00Z", "2026-00-18T08:30:00Z", "2026-13-18T08:30:00Z",
  "2026-07-00T08:30:00Z", "2026-07-32T08:30:00Z",
  "2026-07-18T24:00:00Z", "2026-07-18T08:60:00Z", "2026-07-18T08:30:60Z",
  "2026-07-18T08:30:00+24:00", "2026-07-18T08:30:00+00:60",
  "2026-07-18T8:30:00Z", "2026-07-18t08:30:00z", "2026-07-18T08:30:00,123Z",
  "2026-07-18T08:30:00+0800", "2026-07-18T08:30:00.Z", "2026-07-18T08:30:00Z trailing",
  " 2026-07-18T08:30:00Z", "2026-07-18T08:30:00Z ", "2026-07-18T08:30:00Z\n",
  "0000-01-01T00:00:00Z", "+010000-01-01T00:00:00Z",
  "0001-01-01T00:00:00+00:01", "9999-12-31T23:59:59-00:01",
  "0001-01-01T00:00:00Z", "0001-01-01T08:00:00+08:00",
  "0001-01-01T00:00:00.0000000001Z",
]) {
  assert(Browser.expirationInputValue(value) === "", "invalid persisted timestamp entered editor: " + value);
  assert(Browser.describeExpiration({manualExpiresAt:value}).source === "unknown", "invalid calendar or timezone became account expiry: " + value);
  const fallback = Browser.describeExpiration({manualExpiresAt:value,subscriptionExpiresAt:validSubscription});
  assert(fallback.source === "subscription" && fallback.dateTime === "2026-07-18T08:00:00.000Z", "invalid manual expiry blocked valid subscription: " + value);
  assert(Browser.describeExpiration({expiresAt:value,expiresAtSource:"manual"}).source === "unknown", "invalid effective fallback bypassed validation: " + value);
}
const explicit = Browser.describeExpiration({manualExpiresAt:"invalid",subscriptionExpiresAt:"invalid",expiresAt:validSubscription,expiresAtSource:"manual"});
assert(explicit.source === "manual" && explicit.dateTime === "2026-07-18T08:00:00.000Z", "explicit tagged effective fallback disappeared");
const direct = Browser.describeExpiration({subscriptionExpiresAt:validSubscription,expiresAt:"2026-08-18T08:00:00Z",expiresAtSource:"manual"});
assert(direct.source === "subscription" && direct.dateTime === "2026-07-18T08:00:00.000Z", "effective fallback changed direct-field precedence");
`)
}

func TestFrontendExpirationAcceptsRFC3339NanoOffsetsAndYearBounds(t *testing.T) {
	vm := frontendBrowserVM(t, "accountExpiration")
	frontendPollingRun(t, vm, `
for (const [value, want] of [
  ["2024-02-29T08:30:00Z", "2024-02-29T08:30:00.000Z"],
  ["2000-02-29T08:30:00Z", "2000-02-29T08:30:00.000Z"],
  ["2026-07-18T16:30:00.123456789+08:00", "2026-07-18T08:30:00.123Z"],
  ["2026-07-18T16:30:00.000000001+08:00", "2026-07-18T08:30:00.000Z"],
  ["2026-07-18T16:30:00.123456789123+08:00", "2026-07-18T08:30:00.123Z"],
  ["2026-07-18T08:30:00+23:59", "2026-07-17T08:31:00.000Z"],
  ["2026-07-18T08:30:00-23:59", "2026-07-19T08:29:00.000Z"],
  ["2026-03-08T02:30:00-08:00", "2026-03-08T10:30:00.000Z"],
  ["2026-11-01T01:30:00-07:00", "2026-11-01T08:30:00.000Z"],
  ["2026-11-01T01:30:00-08:00", "2026-11-01T09:30:00.000Z"],
  ["0001-01-01T00:00:01Z", "0001-01-01T00:00:01.000Z"],
  ["0001-01-01T00:00:00.000000001Z", "0001-01-01T00:00:00.000Z"],
  ["0099-01-01T00:00:00Z", "0099-01-01T00:00:00.000Z"],
  ["9999-12-31T23:59:59.999999999Z", "9999-12-31T23:59:59.999Z"],
]) {
  const expiry = Browser.describeExpiration({manualExpiresAt:value});
  assert(expiry.source === "manual" && expiry.dateTime === want, "valid explicit instant changed or was rejected: " + value + " => " + expiry.dateTime);
}
`)
}

func TestFrontendExpirationExactDisplayInTimeZones(t *testing.T) {
	const zoneEnv = "TOSUB2_FRONTEND_EXPIRATION_TEST_ZONE"
	zone := os.Getenv(zoneEnv)
	if zone == "" {
		executable, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		for _, zone := range []string{"UTC", "Asia/Shanghai", "America/Los_Angeles"} {
			t.Run(zone, func(t *testing.T) {
				// Isolate time.Local from other tests, including parallel race tests.
				cmd := exec.Command(executable, "-test.run=^TestFrontendExpirationExactDisplayInTimeZones$", "-test.count=1")
				cmd.Env = append(os.Environ(), zoneEnv+"="+zone)
				if output, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("timezone display: %v\n%s", err, output)
				}
			})
		}
		return
	}
	location, err := time.LoadLocation(zone)
	if err != nil {
		t.Fatal(err)
	}
	oldLocal := time.Local
	time.Local = location
	t.Cleanup(func() { time.Local = oldLocal })
	vm := frontendBrowserVM(t, "accountExpiration")
	if err := vm.Set("testZone", zone); err != nil {
		t.Fatal(err)
	}
	frontendPollingRun(t, vm, `
const displays = {
  "UTC": ["2026-07-18T08:30", "2026-03-08T09:30", "2026-03-08T10:30", "2026-11-01T08:30", "2026-11-01T09:30"],
  "Asia/Shanghai": ["2026-07-18T16:30", "2026-03-08T17:30", "2026-03-08T18:30", "2026-11-01T16:30", "2026-11-01T17:30"],
  "America/Los_Angeles": ["2026-07-18T01:30", "2026-03-08T01:30", "2026-03-08T03:30", "2026-11-01T01:30", "2026-11-01T01:30"],
};
const instants = ["2026-07-18T16:30:00.123456789+08:00", "2026-03-08T01:30:00-08:00", "2026-03-08T02:30:00-08:00", "2026-11-01T01:30:00-07:00", "2026-11-01T01:30:00-08:00"];
assert(Object.hasOwn(displays, testZone), "unrecognized synthetic test zone");
for (let i = 0; i < instants.length; i++) {
  const input = Browser.expirationInputValue(instants[i]);
  assert(input === displays[testZone][i], testZone + " editor instant mismatch: " + input);
  const expiry = Browser.describeExpiration({manualExpiresAt:instants[i]});
  assert(expiry.label === displays[testZone][i].replace("T", " "), testZone + " displayed label mismatch: " + expiry.label);
}
for (const value of ["2026-02-30T08:30:00Z", "2026-07-18T08:00:00", "2026-03-08T02:30:00", "2026-11-01T01:30:00"]) {
  assert(Browser.expirationInputValue(value) === "", testZone + " normalized invalid or timezone-free timestamp");
}
if (testZone === "UTC") assert(Browser.expirationInputValue("0099-01-01T00:00:00Z") === "0099-01-01T00:00", "editor lost four-digit year");
`)
}
