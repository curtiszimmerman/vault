package physical

import (
	"encoding/base64"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/armon/go-metrics"
	"github.com/coreos/go-etcd/etcd"
)

const (
	// Ideally, this prefix would match the "_" used in the file backend, but
	// that prefix has special meaining in etcd. Specifically, it excludes those
	// entries from directory listings.
	EtcdNodeFilePrefix = "."

	// The lock prefix can (and probably should) cause an entry to be excluded
	// from diretory listings, so "_" works here.
	EtcdNodeLockPrefix = "_"

	// The delimiter is the same as the `-C` flag of etcdctl.
	EtcdMachineDelimiter = ","

	// The lock TTL matches the default that Consul API uses, 15 seconds.
	EtcdLockTTL = uint64(15)

	// The ammount of time to wait if a watch fails before trying again.
	EtcdWatchRetryInterval = time.Second

	// The number of times to re-try a failed watch before signaling that leadership is lost.
	EtcdWatchRetryMax = 5
)

var (
	EtcdSyncClusterError         = errors.New("client setup failed: unable to sync etcd cluster")
	EtcdSemaphoreKeysEmptyError  = errors.New("lock queue is empty")
	EtcdLockHeldError            = errors.New("lock already held")
	EtcdLockNotHeldError         = errors.New("lock not held")
	EtcdSemaphoreKeyRemovedError = errors.New("semaphore key removed before lock aquisition")
)

// errorIsMissingKey returns true if the given error is an etcd error with an
// error code corresponding to a missing key.
func errorIsMissingKey(err error) bool {
	etcdErr, ok := err.(*etcd.EtcdError)
	return ok && etcdErr.ErrorCode == 100
}

// EtcdBackend is a physical backend that stores data at specific
// prefix within Etcd. It is used for most production situations as
// it allows Vault to run on multiple machines in a highly-available manner.
type EtcdBackend struct {
	path   string
	client *etcd.Client
}

// newEtcdBackend constructs a etcd backend using a given machine address.
func newEtcdBackend(conf map[string]string) (Backend, error) {
	// Get the etcd path form the configuration.
	path, ok := conf["path"]
	if !ok {
		path = "/vault"
	}

	// Ensure path is prefixed.
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}

	// Set a default machines list and check for an overriding address value.
	machines := "http://128.0.0.1:4001"
	if address, ok := conf["address"]; ok {
		machines = address
	}

	// Create a new client from the supplied addres and attempt to sync with the
	// cluster.
	client := etcd.NewClient(strings.Split(machines, EtcdMachineDelimiter))
	if !client.SyncCluster() {
		return nil, EtcdSyncClusterError
	}

	// Setup the backend.
	return &EtcdBackend{
		path:   path,
		client: client,
	}, nil
}

// Put is used to insert or update an entry.
func (c *EtcdBackend) Put(entry *Entry) error {
	defer metrics.MeasureSince([]string{"etcd", "put"}, time.Now())
	value := base64.StdEncoding.EncodeToString(entry.Value)
	_, err := c.client.Set(c.nodePath(entry.Key), value, 0)
	return err
}

// Get is used to fetch an entry.
func (c *EtcdBackend) Get(key string) (*Entry, error) {
	defer metrics.MeasureSince([]string{"etcd", "get"}, time.Now())

	response, err := c.client.Get(c.nodePath(key), false, false)
	if err != nil {
		if errorIsMissingKey(err) {
			return nil, nil
		}
		return nil, err
	}

	// Decode the stored value from base-64.
	value, err := base64.StdEncoding.DecodeString(response.Node.Value)
	if err != nil {
		return nil, err
	}

	// Construct and return a new entry.
	return &Entry{
		Key:   key,
		Value: value,
	}, nil
}

// Delete is used to permanently delete an entry.
func (c *EtcdBackend) Delete(key string) error {
	defer metrics.MeasureSince([]string{"etcd", "delete"}, time.Now())

	// Remove the key, non-recursively.
	_, err := c.client.Delete(c.nodePath(key), false)
	if err != nil && !errorIsMissingKey(err) {
		return err
	}
	return nil
}

// List is used to list all the keys under a given prefix, up to the next
// prefix.
func (c *EtcdBackend) List(prefix string) ([]string, error) {
	defer metrics.MeasureSince([]string{"etcd", "list"}, time.Now())

	// Set a directory path from the given prefix.
	path := c.nodePathDir(prefix)

	// Get the directory, non-recursively, from etcd. If the directory is
	// missing, we just return an empty list of contents.
	response, err := c.client.Get(path, true, false)
	if err != nil {
		if errorIsMissingKey(err) {
			return []string{}, nil
		}
		return nil, err
	}

	out := make([]string, len(response.Node.Nodes))
	for i, node := range response.Node.Nodes {

		// etcd keys include the full path, so let's trim the prefix directory
		// path.
		name := strings.TrimPrefix(node.Key, path)

		// Check if this node is itself a directory. If it is, add a trailing
		// slash; if it isn't remove the node file prefix.
		if node.Dir {
			out[i] = name + "/"
		} else {
			out[i] = name[1:]
		}
	}
	return out, nil
}

// nodePath returns an etcd filepath based on the given key.
func (b *EtcdBackend) nodePath(key string) string {
	return filepath.Join(b.path, filepath.Dir(key), EtcdNodeFilePrefix+filepath.Base(key))
}

// nodePathDir returns an etcd directory path based on the given key.
func (b *EtcdBackend) nodePathDir(key string) string {
	return filepath.Join(b.path, key) + "/"
}

// nodePathLock returns an etcd directory path used specifically for semaphore
// indicies based on the given key.
func (b *EtcdBackend) nodePathLock(key string) string {
	return filepath.Join(b.path, filepath.Dir(key), EtcdNodeLockPrefix+filepath.Base(key)+"/")
}

// Lock is used for mutual exclusion based on the given key.
func (c *EtcdBackend) LockWith(key, value string) (Lock, error) {
	return &EtcdLock{
		client:          c.client,
		value:           value,
		semaphoreDirKey: c.nodePathLock(key),
	}, nil
}

// EtcdLock emplements a lock using and etcd backend.
type EtcdLock struct {
	client                               *etcd.Client
	value, semaphoreDirKey, semaphoreKey string
	lock                                 sync.Mutex
}

// addSemaphoreKey aquires a new ordered semaphore key.
func (c *EtcdLock) addSemaphoreKey() (string, uint64, error) {
	// CreateInOrder is an atomic operation that can be used to enqueue a
	// request onto a semaphore. In the rest of the comments, we refer to the
	// resulting key as a "semaphore key".
	// https://coreos.com/etcd/docs/2.0.8/api.html#atomically-creating-in-order-keys
	response, err := c.client.CreateInOrder(c.semaphoreDirKey, c.value, EtcdLockTTL)
	if err != nil {
		return "", 0, err
	}
	return response.Node.Key, response.EtcdIndex, nil
}

// getSemaphoreKey determines which semaphore key holder has aquired the lock
// and its value.
func (c *EtcdLock) getSemaphoreKey() (string, string, uint64, error) {
	// Get the list of waiters in order to see if we are next.
	response, err := c.client.Get(c.semaphoreDirKey, true, false)
	if err != nil {
		return "", "", 0, err
	}

	// Make sure the list isn't empty.
	if response.Node.Nodes.Len() == 0 {
		return "", "", response.EtcdIndex, nil
	}
	return response.Node.Nodes[0].Key, response.Node.Nodes[0].Value, response.EtcdIndex, nil
}

// isHeld determines if we are the current holders of the lock.
func (c *EtcdLock) isHeld() (bool, error) {
	if c.semaphoreKey == "" {
		return false, nil
	}

	// Get the key of the curren holder of the lock.
	currentSemaphoreKey, _, _, err := c.getSemaphoreKey()
	if err != nil {
		return false, err
	}
	return c.semaphoreKey == currentSemaphoreKey, nil
}

// assertHeld determines whether or not we are the current holders of the lock
// and returns an EtcdLockNotHeldError if we are not.
func (c *EtcdLock) assertHeld() error {
	held, err := c.isHeld()
	if err != nil {
		return err
	}

	// Check if we don't hold the lock.
	if !held {
		return EtcdLockNotHeldError
	}
	return nil
}

// assertNotHeld determines whether or not we are the current holders of the
// lock and returns an EtcdLockHeldError if we are.
func (c *EtcdLock) assertNotHeld() error {
	held, err := c.isHeld()
	if err != nil {
		return err
	}

	// Check if we hold the lock.
	if held {
		return EtcdLockHeldError
	}
	return nil
}

// watchForKeyRemoval continuously watches a single non-directory key starting
// from the provided etcd index and closes the provided channel when it's
// deleted, expires, or appears to be missing.
func (c *EtcdLock) watchForKeyRemoval(key string, etcdIndex uint64, closeCh chan struct{}) {
	retries := EtcdWatchRetryMax

	for {
		// Start a non-recursive watch of the given key.
		response, err := c.client.Watch(key, etcdIndex, false, nil, nil)
		if err != nil {

			// If the key is just missing, we can exit the loop.
			if errorIsMissingKey(err) {
				break
			}

			// If the error is something else, there's nothing we can do but retry
			// the watch. Check that we still have retries left.
			retries -= 1
			if retries == 0 {
				break
			}

			// Sleep for a period of time to avoid slamming etcd.
			time.Sleep(EtcdWatchRetryInterval)
			continue
		}

		// Check if the key we are concerned with has been removed. If it has, we
		// can exit the loop.
		if response.Node.Key == key &&
			(response.Action == "delete" || response.Action == "expire") {
			break
		}

		// Update the etcd index.
		etcdIndex = response.EtcdIndex + 1
	}

	// Regardless of what happened, we need to close the close channel.
	close(closeCh)
}

// Lock attempts to aquire the lock by waiting for a new semaphore key in etcd
// to become the first in the queue and will block until it is successful or
// it recieves a signal on the provided channel. The returned channel will be
// closed when the lock is lost, either by an explicit call to Unlock or by
// the associated semaphore key in etcd otherwise being deleted or expiring.
//
// If the lock is currently held by this instance of EtcdLock, Lock will
// return an EtcdLockHeldError error.
func (c *EtcdLock) Lock(stopCh <-chan struct{}) (<-chan struct{}, error) {
	// Get the local lock before interacting with etcd.
	c.lock.Lock()
	defer c.lock.Unlock()

	// Check if the lock is already held.
	if err := c.assertNotHeld(); err != nil {
		return nil, err
	}

	// Add a new semaphore key that we will track.
	semaphoreKey, _, err := c.addSemaphoreKey()
	if err != nil {
		return nil, err
	}
	c.semaphoreKey = semaphoreKey

	// Get the current semaphore key.
	currentSemaphoreKey, _, currentEtcdIndex, err := c.getSemaphoreKey()
	if err != nil {
		return nil, err
	}

	// Create an etcd-compatible boolean stop channel from the provided
	// interface stop channel.
	boolStopCh := make(chan bool)
	go func() {
		<-stopCh
		close(boolStopCh)
	}()

	// Loop until the we current semaphore key matches ours.
	for semaphoreKey != currentSemaphoreKey {
		var err error

		// Start a watch of the entire lock directory, providing the stop channel.
		response, err := c.client.Watch(c.semaphoreDirKey, currentEtcdIndex+1, true, nil, boolStopCh)
		if err != nil {

			// If the error is not an etcd error, we can assume it's a notification
			// of the stop channel having closed. In this scenario, we also want to
			// remove our semaphore key as we are no longer waiting to aquire the
			// lock.
			if _, ok := err.(*etcd.EtcdError); !ok {
				_, err = c.client.Delete(c.semaphoreKey, false)
			}
			return nil, err
		}

		// Make sure the index we are waiting for has not been removed. If it has,
		// this is an error and nothing else needs to be done.
		if response.Node.Key == semaphoreKey &&
			(response.Action == "delete" || response.Action == "expire") {
			return nil, EtcdSemaphoreKeyRemovedError
		}

		// Get the current semaphore key and etcd index.
		currentSemaphoreKey, _, currentEtcdIndex, err = c.getSemaphoreKey()
		if err != nil {
			return nil, err
		}
	}

	// Create a channel to signal when we lose the lock.
	done := make(chan struct{})
	go c.watchForKeyRemoval(c.semaphoreKey, currentEtcdIndex+1, done)
	return done, nil
}

// Unlock releases the lock by deleting the associated semaphore key in etcd.
//
// If the lock is not currently held by this instance of EtcdLock, Unlock will
// return an EtcdLockNotHeldError error.
func (c *EtcdLock) Unlock() error {
	// Get the local lock before interacting with etcd.
	c.lock.Lock()
	defer c.lock.Unlock()

	// Check that the lock is held.
	if err := c.assertHeld(); err != nil {
		return err
	}

	// Delete our semaphore key.
	if _, err := c.client.Delete(c.semaphoreKey, false); err != nil {
		return err
	}
	return nil
}

// Value checks whether or not the lock is held by any instance of EtcdLock,
// including this one, and returns the current value.
func (c *EtcdLock) Value() (bool, string, error) {
	semaphoreKey, semaphoreValue, _, err := c.getSemaphoreKey()
	if err != nil {
		return false, "", err
	}

	if semaphoreKey == "" {
		return false, "", nil
	}
	return true, semaphoreValue, nil
}
