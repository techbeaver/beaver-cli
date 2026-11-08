package credential

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestLockGivesOneHolderAtATime(t *testing.T) {
	dir := t.TempDir()
	var inside, maxInside, total int32
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			release, err := Lock(ctx, dir, "default")
			if err != nil {
				t.Error(err)
				return
			}
			n := atomic.AddInt32(&inside, 1)
			for {
				m := atomic.LoadInt32(&maxInside)
				if n <= m || atomic.CompareAndSwapInt32(&maxInside, m, n) {
					break
				}
			}
			time.Sleep(2 * time.Millisecond)
			atomic.AddInt32(&total, 1)
			atomic.AddInt32(&inside, -1)
			release()
		}()
	}
	wg.Wait()
	if maxInside != 1 {
		t.Fatalf("%d holders were inside the critical section at once", maxInside)
	}
	if total != 12 {
		t.Fatalf("only %d of 12 got the lock", total)
	}
}

func TestLockWaitEndsWithTheContext(t *testing.T) {
	dir := t.TempDir()
	release, err := Lock(context.Background(), dir, "default")
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := Lock(ctx, dir, "default"); err == nil {
		t.Fatal("a held lock was taken twice")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("the wait ignored its deadline")
	}
}

func TestLockIsPerProfileAndReleasable(t *testing.T) {
	dir := t.TempDir()
	a, err := Lock(context.Background(), dir, "a")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	b, err := Lock(ctx, dir, "b")
	if err != nil {
		t.Fatalf("another profile was blocked: %v", err)
	}
	b()
	a()
	a() // releasing twice is harmless
	again, err := Lock(ctx, dir, "a")
	if err != nil {
		t.Fatalf("the lock was not released: %v", err)
	}
	again()
}

func TestLockCreatesTheDirectoryPrivately(t *testing.T) {
	dir := t.TempDir() + "/nested/config"
	release, err := Lock(context.Background(), dir, "default")
	if err != nil {
		t.Fatal(err)
	}
	release()
	info, err := osStat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("directory mode %o is readable by others", info.Mode().Perm())
	}
}

func TestLockRefusesAnUnusableDirectory(t *testing.T) {
	file := t.TempDir() + "/f"
	if err := writeFile(file); err != nil {
		t.Fatal(err)
	}
	if _, err := Lock(context.Background(), file+"/sub", "default"); err == nil {
		t.Fatal("a lock under a regular file was granted")
	}
}
