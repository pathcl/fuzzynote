package service

import (
	"context"
)

type RefreshKey struct {
	AllowOverride bool
	ChangedKeys   map[string]struct{}
}

type FinishWithPurgeError struct{}

func (e FinishWithPurgeError) Error() string {
	return ""
}

type namedWal struct {
	name string
	wal  []EventLog
}

// Start begins push/pull for all WalFiles
func (r *DBListRepo) Start(client Client) error {
	inputEvtsChan := make(chan interface{})

	ctx, cancel := context.WithCancel(context.Background())

	replayChan := make(chan namedWal)
	//reorderAndReplayChan := make(chan []EventLog)

	// We need atomicity between wal pull/replays and handling of keypress events, as we need
	// events to operate on a predictable state (rather than a keypress being applied to state
	// that differs from when the user intended due to async updates).
	// Therefore, we consume client events into a channel, and consume from it in the same loop
	// as the pull/replay loop.
	errChan := make(chan error)
	go func() {
		for {
			select {
			case n := <-replayChan:
				name, wal := n.name, n.wal
				if err := r.Replay(wal); err != nil {
					errChan <- err
					return
				}
				if name != "" {
					r.setProcessedWalChecksum(name)
				}
				changedKeys, allowOverride := getChangedListItemKeysFromWal(wal)
				go func() {
					inputEvtsChan <- RefreshKey{
						ChangedKeys:   changedKeys,
						AllowOverride: allowOverride,
					}
				}()
			case ev := <-r.remoteCursorMoveChan:
				// Update active key position of collaborator if changes have occurred
				updated := r.SetCollabPosition(ev)
				if updated {
					go func() {
						inputEvtsChan <- RefreshKey{}
					}()
				}
			case ev := <-inputEvtsChan:
				if err := client.HandleEvent(ev); err != nil {
					cancel()
					<-r.finalFlushChan
					_, isPurge := err.(FinishWithPurgeError)
					if finishErr := r.finish(isPurge); finishErr != nil {
						errChan <- finishErr
					}
					errChan <- err
					return
				}
			}
		}
	}()

	// To avoid blocking key presses on the main processing loop, run heavy sync ops in a separate
	// loop, and only add to channel for processing if there's any changes that need syncing
	// This is run after the goroutine above is triggered to ensure a thread is consuming from replayChan
	err := r.startSync(ctx, replayChan, inputEvtsChan)
	if err != nil {
		return err
	}

	// This is the main loop of operation in the app.
	// We consume all term events into our own channel (handled above).
	// This is handled in a separate goroutine due to pontential contention in the loop above, whereby we consume
	// from inputEvtsChan and publish to the errChan (rather than a single select with two options in the main thread),
	// e.g.
	// select {
	// case inputEvtsChan <- client.AwaitEvent():
	// case err <- errchan:
	//     ...
	// }
	go func() {
		for {
			ev := client.AwaitEvent()
			go func() {
				inputEvtsChan <- ev
			}()
		}
	}()
	return <-errChan
}

func (r *DBListRepo) registerWeb() error {
	if err := r.web.establishWebSocketConnection(); err != nil {
		return err
	}

	if r.email == "" {
		if pong, err := r.web.ping(); err == nil {
			r.setEmail(pong.User)
			r.web.tokens.SetEmail(pong.User)
			r.web.tokens.Flush()
		}
	}

	r.DeleteWalFile(string(r.email))
	r.AddWalFile(
		&WebWalFile{
			uuid: string(r.email),
			web:  r.web,
		},
		true,
	)

	return nil
}

func (r *DBListRepo) AddWalFile(wf WalFile, hasFullAccess bool) {
	r.allWalFileMut.Lock()
	r.allWalFiles[wf.GetUUID()] = wf
	r.allWalFileMut.Unlock()

	if hasFullAccess {
		r.syncWalFileMut.Lock()
		r.syncWalFiles[wf.GetUUID()] = wf
		r.syncWalFileMut.Unlock()
	}

	if _, ok := wf.(*WebWalFile); ok {
		r.webWalFileMut.Lock()
		r.webWalFiles[wf.GetUUID()] = wf
		r.webWalFileMut.Unlock()
	}
}

func (r *DBListRepo) DeleteWalFile(name string) {
	r.allWalFileMut.Lock()
	r.syncWalFileMut.Lock()
	r.webWalFileMut.Lock()
	defer r.allWalFileMut.Unlock()
	defer r.syncWalFileMut.Unlock()
	defer r.webWalFileMut.Unlock()
	delete(r.allWalFiles, name)
	delete(r.syncWalFiles, name)
	delete(r.webWalFiles, name)
}
