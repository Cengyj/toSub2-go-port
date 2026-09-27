package transport

import (
	"encoding/json"
	"net/http"
	"testing"
)

func TestPartitionedCookieCheckpointRetainsIsolation(t *testing.T) {
	ordinary := Cookie{Name: "sid", Value: "ordinary", Domain: "auth.example", Path: "/", Secure: true}
	a := ordinary
	a.Value = "first"
	a.PartitionKey = &CookiePartitionKey{TopLevelSite: "https://first.example", HasCrossSiteAncestor: true}
	b := a
	b.Value = "second"
	b.PartitionKey = &CookiePartitionKey{TopLevelSite: "https://second.example", HasCrossSiteAncestor: true}
	jar := NewCookieJar(ordinary, a, b)
	if len(jar.Cookies()) != 3 {
		t.Fatal("partitioned cookies overwrote another site or the ordinary cookie")
	}
	if got := jar.HeaderFor("https://auth.example/"); got != "sid=ordinary" {
		t.Fatalf("partitioned cookie leaked without top-level context: %s", got)
	}
	values := jar.Cookies()
	values[1].PartitionKey.TopLevelSite = "https://mutated.example"
	if jar.Cookies()[1].PartitionKey.TopLevelSite != "https://first.example" {
		t.Fatal("cookie snapshot aliases internal key")
	}
	wire, err := json.Marshal(jar.Cookies())
	if err != nil {
		t.Fatal(err)
	}
	var restored []Cookie
	if err = json.Unmarshal(wire, &restored); err != nil {
		t.Fatal(err)
	}
	next := NewCookieJar(restored...)
	if len(next.Cookies()) != 3 || next.Cookies()[2].PartitionKey.TopLevelSite != "https://second.example" {
		t.Fatal("partition key lost in checkpoint")
	}
	deleted := next.Cookies()[1]
	deleted.Expires = 1
	next.Set(deleted)
	if len(next.Cookies()) != 2 || next.HeaderFor("https://auth.example/") != "sid=ordinary" {
		t.Fatal("partition deletion removed another cookie")
	}
}

func TestSetCookieDoesNotDowngradeUnknownPartition(t *testing.T) {
	jar := NewCookieJar()
	jar.SetFromResponse("https://auth.example/", http.Header{"Set-Cookie": {"secure=value; Secure; Path=/; Partitioned"}})
	if got := jar.Cookies(); len(got) != 1 || !got[0].Partitioned {
		t.Fatal("partition attribute was dropped")
	}
	if jar.HeaderFor("https://auth.example/") != "" {
		t.Fatal("partitioned cookie sent as unpartitioned")
	}
	if _, ok := ParseSetCookie("insecure=value; Partitioned", "https://auth.example/"); ok {
		t.Fatal("insecure partitioned cookie accepted")
	}
}
