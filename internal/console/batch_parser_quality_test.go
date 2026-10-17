package console

import (
	"encoding/json"
	"testing"
)

func BenchmarkSmartAccount(b *testing.B) {
	for name, line := range map[string]string{
		"structured": "quality@example.test----synthetic-password----https://mail.example.test/messages----JBSWY3DPEHPK3PXP",
		"unordered":  "https://mail.example.test/messages|JBSWY3DPEHPK3PXP|synthetic-password|quality@example.test",
		"ambiguous":  "quality@example.test----password|https://mail.example.test/messages|JBSWY3DPEHPK3PXP",
	} {
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				_, _ = smartAccount(line)
			}
		})
	}
}

// FuzzSmartAccount guards the import boundary without submitting a job or
// reading credentials. Ambiguous input must remain deterministic and harmless.
func FuzzSmartAccount(f *testing.F) {
	for _, line := range []string{
		"quality@example.test",
		"quality@example.test----synthetic-password----JBSWY3DPEHPK3PXP",
		"https://mail.example.test/messages|JBSWY3DPEHPK3PXP|synthetic-password|quality@example.test",
		"one@example.test----two@example.test",
		"quality@example.test--------JBSWY3DPEHPK3PXP",
		"\x00|；::----",
	} {
		f.Add(line)
	}
	f.Fuzz(func(t *testing.T, line string) {
		first, err := smartAccount(line)
		second, again := smartAccount(line)
		if (err == nil) != (again == nil) {
			t.Fatal("nondeterministic import acceptance")
		}
		if err != nil {
			return
		}
		if !isEmail(first.Email) || first.Input == nil {
			t.Fatal("accepted account lacks an independent email or input")
		}
		a, ea := json.Marshal(first)
		b, eb := json.Marshal(second)
		if ea != nil || eb != nil || string(a) != string(b) {
			t.Fatal("nondeterministic parsed account")
		}
	})
}
