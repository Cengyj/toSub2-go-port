package jobs

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/store"
)

// benchmarkCredentialStore is entirely synthetic: no OS credential service or
// real account is consulted. Delayed calls model a slow protected store while
// separately recording the time actually spent in those calls.
type benchmarkCredentialStore struct {
	values  map[string]credentials.Credentials
	delay   time.Duration
	elapsed time.Duration
	calls   int
}

func (v *benchmarkCredentialStore) measure() func() {
	started := time.Now()
	if v.delay > 0 {
		time.Sleep(v.delay)
	}
	return func() {
		v.elapsed += time.Since(started)
		v.calls++
	}
}
func (v *benchmarkCredentialStore) Load(email string) (credentials.Credentials, error) {
	defer v.measure()()
	value, ok := v.values[email]
	if !ok {
		return credentials.Credentials{}, os.ErrNotExist
	}
	return value, nil
}
func (v *benchmarkCredentialStore) Save(email string, value credentials.Credentials) error {
	defer v.measure()()
	v.values[email] = value
	return nil
}
func (v *benchmarkCredentialStore) Delete(email string) error {
	defer v.measure()()
	delete(v.values, email)
	return nil
}

// QueueOperation serializes eligibility, credential updates and durable
// metadata under one mutex. These cases quantify their costs independently
// without starting the scheduler or authenticating any account.
func BenchmarkManagerQueueOperation(b *testing.B) {
	for _, count := range []int{1, 100} {
		for _, mode := range []struct {
			name        string
			disk, vault bool
			delay       time.Duration
		}{
			{name: "memory"},
			{name: "memory-vault", vault: true},
			{name: "atomic-metadata-vault", disk: true, vault: true},
			{name: "atomic-metadata-vault-1ms", disk: true, vault: true, delay: time.Millisecond},
		} {
			b.Run(fmt.Sprintf("%s/accounts-%d", mode.name, count), func(b *testing.B) {
				m := NewManager(context.Background(), 1, nil, nil)
				b.Cleanup(m.cancel)
				ids := make([]string, count)
				for i := range ids {
					job, err := m.Submit(Spec{ID: fmt.Sprintf("fixture-%04d", i), Email: fmt.Sprintf("fixture-%04d@example.test", i), ImportOnly: true})
					if err != nil {
						b.Fatal(err)
					}
					ids[i] = job.ID
				}
				if mode.disk {
					var err error
					m.store, err = store.New(b.TempDir())
					if err != nil {
						b.Fatal(err)
					}
				}
				vault := &benchmarkCredentialStore{values: make(map[string]credentials.Credentials), delay: mode.delay}
				if mode.vault {
					m.SetCredentialStore(vault)
				}
				values := map[string]any{"password": "Synthetic-benchmark-password9!"}
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					if _, err := m.QueueOperation(ids[i%count], "relogin", values); err != nil {
						b.Fatal(err)
					}
					if m.lastPersistError != nil {
						b.Fatal(m.lastPersistError)
					}
				}
				b.StopTimer()
				b.ReportMetric(float64(vault.calls)/float64(b.N), "vault-calls/op")
				b.ReportMetric(float64(vault.elapsed.Nanoseconds())/float64(b.N), "vault-ns/op")
			})
		}
	}
}
