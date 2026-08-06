package recorder

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
)

func TestCloseDrainsAndIsConcurrentSafe(t *testing.T) {
	recorder := NewMemory()
	const records = 1000
	for i := 0; i < records; i++ {
		if err := recorder.Write(&Record{RequestID: "request"}); err != nil {
			t.Fatal(err)
		}
	}

	const closers = 32
	errs := make(chan error, closers)
	var wg sync.WaitGroup
	for i := 0; i < closers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- recorder.Close()
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("Close returned %v", err)
		}
	}
	if got := len(recorder.Records()); got != records {
		t.Fatalf("drained %d records, want %d", got, records)
	}
	if err := recorder.Write(&Record{}); !errors.Is(err, ErrClosed) {
		t.Fatalf("Write after Close = %v, want ErrClosed", err)
	}
}

func TestWriteConcurrentWithCloseDoesNotPanicOrLoseAcceptedRecords(t *testing.T) {
	recorder := NewMemory()
	const writers = 64
	const perWriter = 200
	start := make(chan struct{})
	var accepted atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for j := 0; j < perWriter; j++ {
				err := recorder.Write(&Record{RequestID: "request"})
				if err == nil {
					accepted.Add(1)
					continue
				}
				if !errors.Is(err, ErrClosed) {
					t.Errorf("Write returned %v", err)
				}
				return
			}
		}()
	}
	close(start)
	if err := recorder.Close(); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	if got, want := int64(len(recorder.Records())), accepted.Load(); got != want {
		t.Fatalf("drained %d records, accepted %d", got, want)
	}
}
