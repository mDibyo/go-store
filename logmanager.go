/*
Package gostore implements a simple single-node log-based key-value
store. It supports multiple concurrent transactions through a set of
locks on values.
*/
package gostore

import (
	"flag"
	"fmt"
	"github.com/golang/protobuf/proto"
	pb "github.com/mDibyo/gostore/pb"
	"io/ioutil"
	"sync"
)

// Key represents a key in the store
type Key string

// Value represents the value for a key in the key store
type Value []byte

type storeMapValue struct {
	value Value

	// RWMutex attributes
	lock sync.RWMutex

	// ValueAccessor attributes
	rAccessorChan chan *valueAccessor
	wAccessorChan chan *valueAccessor
	ping          chan struct{}
}

func newStoreMapValue() *storeMapValue {
	return &storeMapValue{
		rAccessorChan: make(chan *valueAccessor),
		wAccessorChan: make(chan *valueAccessor),
		ping:          make(chan struct{}),
	}
}

// rwMutexWrapper is a thread-safe convenience wrapper for sync.RWMutex used in StoreMapValue.
type rwMutexWrapper struct {
	selfLock sync.Mutex    // Self Lock to synchronize lock and unlock operations.
	smvLock  *sync.RWMutex // the lock being wrapped.
	held     bool          // Whether the lock is held.
	wAllowed bool          // Whether writes are allowed.
}

func wrapRWMutex(l *sync.RWMutex) rwMutexWrapper {
	return rwMutexWrapper{smvLock: l}
}

func (rw *rwMutexWrapper) rLocked() (b bool) {
	rw.selfLock.Lock()
	b = rw.held && !rw.wAllowed
	rw.selfLock.Unlock()
	return
}

func (rw *rwMutexWrapper) wLocked() (b bool) {
	rw.selfLock.Lock()
	b = rw.held && rw.wAllowed
	rw.selfLock.Unlock()
	return
}

func (rw *rwMutexWrapper) rLock() {
	rw.selfLock.Lock()
	defer rw.selfLock.Unlock()

	if rw.held {
		return
	}
	rw.rLockUnsafe()
}

func (rw *rwMutexWrapper) rLockUnsafe() {
	rw.smvLock.RLock()
	rw.held = true
}

func (rw *rwMutexWrapper) rUnlock() {
	rw.selfLock.Lock()
	defer rw.selfLock.Unlock()

	if !rw.held {
		return
	}
	rw.rUnlockUnsafe()
}

func (rw *rwMutexWrapper) rUnlockUnsafe() {
	rw.smvLock.RUnlock()
	rw.held = false
}

func (rw *rwMutexWrapper) wLock() {
	rw.selfLock.Lock()
	defer rw.selfLock.Unlock()

	if rw.held && rw.wAllowed {
		return
	}
	rw.wLockUnsafe()
}

func (rw *rwMutexWrapper) wLockUnsafe() {
	rw.smvLock.Lock()
	rw.held = true
	rw.wAllowed = true
}

func (rw *rwMutexWrapper) wUnlock() {
	rw.selfLock.Lock()
	defer rw.selfLock.Unlock()

	if !rw.held || !rw.wAllowed {
		return
	}
	rw.wUnlockUnsafe()
}

func (rw *rwMutexWrapper) wUnlockUnsafe() {
	rw.smvLock.Unlock()
	rw.held = false
	rw.wAllowed = false
}

func (rw *rwMutexWrapper) promote() {
	rw.selfLock.Lock()
	defer rw.selfLock.Unlock()

	if rw.wAllowed {
		return
	}
	rw.rUnlockUnsafe()
	rw.wLockUnsafe()
}

func (rw *rwMutexWrapper) unlock() {
	rw.selfLock.Lock()
	defer rw.selfLock.Unlock()

	if !rw.held {
		return
	}

	if rw.wAllowed {
		rw.wUnlockUnsafe()
	} else {
		rw.rUnlockUnsafe()
	}
}

// TransactionID is used to uniquely identify/represent a transaction.
type TransactionID int64

type storeMap map[Key]*storeMapValue

func (sm storeMap) storeMapValue(k Key, createIfNotExist bool) (smv *storeMapValue, err error) {
	smv, ok := sm[k]
	if ok {
		return
	}
	if !createIfNotExist {
		return smv, fmt.Errorf("key %s does not exist.", k)
	}

	smv = newStoreMapValue()
	sm[k] = smv
	return
}

type currentMutexesMap map[Key]*rwMutexWrapper

func (cm currentMutexesMap) getWrappedRWMutex(k Key, smv *storeMapValue) *rwMutexWrapper {
	if rw, ok := cm[k]; ok {
		return rw
	}
	_rw := wrapRWMutex(&smv.lock)
	cm[k] = &_rw
	return &_rw
}

var logFileFmt = "%012d_%012d.log"

type logManager struct {
	log            pb.Log                              // the log of transaction operations
	logDir         string                              // the directory in which log is stored
	logLock        sync.Mutex                          // lock to synchronize access to the log
	nextLSN        int                                 // the LSN for the next log entry
	nextLSNToFlush int                                 // the LSN of the next log entry to be flushed
	nextTID        TransactionID                       // the Transaction ID for the next transaction
	currMutexes    map[TransactionID]currentMutexesMap // the mutexes held currently by running transactions
	store          storeMap                            // the master copy of the current state of the store
}

func newLogManager(ld string) (lm *logManager, err error) {
	lm = &logManager{}
	lm.logDir = ld
	if lm.logDir == "" {
		lm.logDir = "./data"
	}
	lm.currMutexes = make(map[TransactionID]currentMutexesMap)
	lm.store = make(storeMap)

	// Retrieve old logs if they exist and replay
	files, err := ioutil.ReadDir(lm.logDir)
	if err != nil {
		return nil, fmt.Errorf("could not retrieve old logs: %v", err)
	}
	for _, file := range files {
		if !file.IsDir() {
			var startLSN, endLSN = -1, -1
			_, err := fmt.Sscanf(file.Name(), logFileFmt, &startLSN, &endLSN)
			if err != nil {
				continue
			}
			if startLSN != lm.nextLSN || endLSN < startLSN {
				err = fmt.Errorf("log file %s was not in the expected format", file.Name())
				break
			}
			filename := fmt.Sprintf("%s/%s", lm.logDir, file.Name())
			data, err := ioutil.ReadFile(filename)
			if err != nil {
				err = fmt.Errorf("could not read log file %s: %v", filename, err)
				break
			}
			if err = proto.UnmarshalMerge(data, &lm.log); err != nil {
				err = fmt.Errorf("could not unmarshal log file %s: %v", filename, err)
				break
			}
			lm.nextLSN = len(lm.log.Entry)
			if nextLSN := endLSN + 1; nextLSN != lm.nextLSN {
				err = fmt.Errorf("log file %s did not have the right number of entries", filename)
				break
			}
		}
	}
	lm.nextLSNToFlush = lm.nextLSN
	return
}

func (lm *logManager) addLogEntry(e *pb.LogEntry) {
	lm.logLock.Lock()
	defer lm.logLock.Unlock()

	entries := &lm.log.Entry
	e.Lsn = proto.Int64(int64(lm.nextLSN))
	*entries = append(*entries, e)
	lm.nextLSN++
}

func (lm *logManager) flushLog() error {
	lm.logLock.Lock()
	defer lm.logLock.Unlock()

	entries := lm.log.GetEntry()
	logToFlush := &pb.Log{
		Entry: entries[lm.nextLSNToFlush:],
	}
	data, err := proto.Marshal(logToFlush)
	if err != nil {
		return fmt.Errorf("error while marshalling log to be flushed: %v", err)
	}
	filename := fmt.Sprintf(logFileFmt, lm.nextLSNToFlush, lm.nextLSN-1)
	if err := ioutil.WriteFile(fmt.Sprintf("%s/%s", lm.logDir, filename), data, 0644); err != nil {
		return fmt.Errorf("error while writing out log: %v", err)
	}
	lm.nextLSNToFlush = lm.nextLSN
	return nil
}

func (lm *logManager) nextTransactionID() TransactionID {
	lm.nextTID++
	return lm.nextTID - 1
}

func (lm *logManager) beginTransaction(tid TransactionID) {
	lm.addLogEntry(&pb.LogEntry{
		Tid:       proto.Int64(int64(tid)),
		EntryType: pb.LogEntry_BEGIN.Enum(),
	})
	lm.currMutexes[tid] = make(currentMutexesMap)
}

func (lm *logManager) getValue(tid TransactionID, k Key) (Value, error) {
	cm, ok := lm.currMutexes[tid]
	if !ok {
		return nil, fmt.Errorf("transaction with ID %d is not currently running", tid)
	}
	smv, err := lm.store.storeMapValue(k, false)
	if err != nil {
		return nil, fmt.Errorf("could not retrieve value: %v", err)
	}

	rw := cm.getWrappedRWMutex(k, smv)
	rw.rLock()
	return smv.value, nil
}

func (lm *logManager) setValue(tid TransactionID, k Key, v Value) error {
	cm, ok := lm.currMutexes[tid]
	if !ok {
		return fmt.Errorf("transaction with ID %d is not currently running.", tid)
	}
	if v == nil {
		return fmt.Errorf("value is nil.")
	}
	// Add key if it does not exist
	smv, err := lm.store.storeMapValue(k, true)
	if err != nil {
		return fmt.Errorf("could not retrieve value: %v", err)
	}

	// Update value
	rw := cm.getWrappedRWMutex(k, smv)
	rw.wLock()
	var oldValue []byte
	if smv.value != nil {
		oldValue = CopyByteArray(smv.value)
	}
	newValue := CopyByteArray(v)
	smv.value = v

	// Write log entry
	lm.addLogEntry(&pb.LogEntry{
		Tid:       proto.Int64(int64(tid)),
		EntryType: pb.LogEntry_UPDATE.Enum(),
		Key:       proto.String(string(k)),
		OldValue:  oldValue,
		NewValue:  newValue,
	})

	return nil
}

func (lm *logManager) deleteValue(tid TransactionID, k Key) error {
	cm, ok := lm.currMutexes[tid]
	if !ok {
		return fmt.Errorf("transaction with ID %d is not currently running.", tid)
	}
	smv, err := lm.store.storeMapValue(k, false)
	if err != nil {
		return fmt.Errorf("could not retrieve value: %v", err)
	}

	// Delete key
	rw := cm.getWrappedRWMutex(k, smv)
	rw.wLock()
	oldValue := CopyByteArray(smv.value)
	var newValue []byte
	delete(lm.store, k)

	// Write log entry
	lm.addLogEntry(&pb.LogEntry{
		Tid:       proto.Int64(int64(tid)),
		EntryType: pb.LogEntry_UPDATE.Enum(),
		Key:       proto.String(string(k)),
		OldValue:  oldValue,
		NewValue:  newValue,
	})

	return nil
}

func (lm *logManager) commitTransaction(tid TransactionID) error {
	cm, ok := lm.currMutexes[tid]
	if !ok {
		return fmt.Errorf("transaction with ID %d is not currently running", tid)
	}

	// Write out COMMIT and END log entries
	lm.addLogEntry(&pb.LogEntry{
		Tid:       proto.Int64(int64(tid)),
		EntryType: pb.LogEntry_COMMIT.Enum(),
	})

	lm.addLogEntry(&pb.LogEntry{
		Tid:       proto.Int64(int64(tid)),
		EntryType: pb.LogEntry_END.Enum(),
	})

	// Flush out log
	if err := lm.flushLog(); err != nil {
		return fmt.Errorf("error while flushing log: %v", err)
	}

	// Release all locks and remove from current transactions
	for _, rw := range cm {
		rw.unlock()
	}
	delete(lm.currMutexes, tid)
	return nil
}

func (lm *logManager) abortTransaction(tid TransactionID) (err error) {
	cm, ok := lm.currMutexes[tid]
	if !ok {
		err = fmt.Errorf("transaction with ID %d is not currently running", tid)
		return
	}

	// Write out ABORT entry
	lm.addLogEntry(&pb.LogEntry{
		Tid:       proto.Int64(int64(tid)),
		EntryType: pb.LogEntry_ABORT.Enum(),
	})

	// Undo updates (and write log entries)

	entries := &lm.log.Entry
	iterateEntries := (*entries)[:]
iterate:
	for i := len(iterateEntries) - 1; i >= 0; i-- {
		e := iterateEntries[i]
		if *e.Tid == int64(tid) {
			switch *e.EntryType {
			case pb.LogEntry_UPDATE: // Undo UPDATE records
				k := Key(*e.Key)
				smv, err := lm.store.storeMapValue(k, false)
				if err != nil {
					return fmt.Errorf("could not retrieve value: %v", err)
				}
				rw := cm.getWrappedRWMutex(k, smv)
				if err != nil {
					return err
				}
				rw.wLock()
				smv.value = CopyByteArray(e.OldValue)
				lm.addLogEntry(&pb.LogEntry{
					Tid:       proto.Int64(int64(tid)),
					EntryType: pb.LogEntry_UNDO.Enum(),
					Key:       e.Key,
					OldValue:  e.NewValue,
					NewValue:  e.OldValue,
					UndoLsn:   e.Lsn,
				})
			case pb.LogEntry_BEGIN: // Stop when BEGIN record is reached
				break iterate
			}
		}
	}

	lm.addLogEntry(&pb.LogEntry{
		Tid:       proto.Int64(int64(tid)),
		EntryType: pb.LogEntry_END.Enum(),
	})

	// Flush out log
	lm.flushLog()

	// Release all locks and remove from current transactions
	for _, rw := range cm {
		rw.unlock()
	}
	delete(lm.currMutexes, tid)
	return
}

var lmInstance logManager

func init() {
	logDir := flag.String("logDir", "", "the directory in which log files will be stored")
	flag.Parse()
	if lmInstancePtr, err := newLogManager(*logDir); err != nil {
		panic(err)
	} else {
		lmInstance = *lmInstancePtr
	}
}
