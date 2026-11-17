package main

import (
	"context"
	json "encoding/json/v2"
	"errors"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"
)

const maxState = 32 << 20

type turn struct {
	Index      int64     `json:"turn_index"`
	Speaker    string    `json:"speaker"`
	OccurredAt time.Time `json:"occurred_at"`
	Body       string    `json:"body"`
}
type queuedTurn struct {
	Graph        string `json:"graph"`
	Endpoint     string `json:"endpoint"`
	Conversation string `json:"conversation"`
	Turn         turn   `json:"turn"`
}
type rejection struct {
	Index  int64  `json:"turn_index"`
	Reason string `json:"reason"`
}
type pauseInterval struct {
	Start time.Time  `json:"start"`
	End   *time.Time `json:"end"`
}

type sessionState struct {
	Next           int64           `json:"next"`
	Paused         bool            `json:"paused"`
	PauseIntervals []pauseInterval `json:"pause_intervals,omitempty"`
	Pending        []queuedTurn    `json:"pending"`
	Rejected       []rejection     `json:"rejected"`
}

func withState(dir, name string, fn func(*sessionState, func() error) error) error {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return errors.New("state directory")
	}
	lockPath := filepath.Join(dir, name+".lock")
	fd, err := unix.Open(lockPath, unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return err
	}
	lock := os.NewFile(uintptr(fd), lockPath)
	defer lock.Close()
	if err := unix.Flock(fd, unix.LOCK_EX); err != nil {
		return err
	}
	defer unix.Flock(fd, unix.LOCK_UN)
	path := filepath.Join(dir, name+".json")
	var state sessionState
	info, err = os.Lstat(path)
	if err == nil {
		if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > maxState {
			return errors.New("state file")
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if err := json.Unmarshal(data, &state, json.RejectUnknownMembers(true)); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	save := func() error {
		data, err := json.Marshal(state)
		if err != nil {
			return err
		}
		if len(data) > maxState {
			return errors.New("state full")
		}
		file, err := os.CreateTemp(dir, ".ingest-*")
		if err != nil {
			return err
		}
		name := file.Name()
		defer os.Remove(name)
		if _, err = file.Write(data); err == nil {
			err = file.Sync()
		}
		if closeErr := file.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			return err
		}
		if err := os.Rename(name, path); err != nil {
			return err
		}
		parent, err := os.Open(dir)
		if err != nil {
			return err
		}
		err = parent.Sync()
		return errors.Join(err, parent.Close())
	}
	return fn(&state, save)
}

// wasPausedAt은 훅이 잠금 전에 시작한 시점의 중지 상태를 판정한다.
func (s *sessionState) wasPausedAt(at time.Time) bool {
	if s.Paused && (len(s.PauseIntervals) == 0 || time.Now().UTC().Before(s.PauseIntervals[len(s.PauseIntervals)-1].Start)) {
		return true
	}
	for _, interval := range s.PauseIntervals {
		if !at.Before(interval.Start) && (interval.End == nil || at.Before(*interval.End)) {
			return true
		}
	}
	return false
}

// withDelivery는 같은 세션의 전송 회차만 직렬화한다. 대기는 상한을 두고 실패하면 대기 항목을 보존한다.
func withDelivery(ctx context.Context, dir, name string, fn func() error) error {
	path := filepath.Join(dir, name+".delivery.lock")
	fd, err := unix.Open(path, unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return err
	}
	file := os.NewFile(uintptr(fd), path)
	defer file.Close()
	waitCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	for {
		err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
			return err
		}
		timer := time.NewTimer(100 * time.Millisecond)
		select {
		case <-waitCtx.Done():
			timer.Stop()
			return waitCtx.Err()
		case <-timer.C:
		}
	}
	defer unix.Flock(fd, unix.LOCK_UN)
	return fn()
}
