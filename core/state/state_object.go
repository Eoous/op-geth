// Copyright 2014 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package state

import (
	"bytes"
	"fmt"
	"io"
	"slices"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/core/opcodeCompiler/compiler"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/metrics"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/ethereum/go-ethereum/trie/trienode"
	"github.com/holiman/uint256"
)

type Code []byte

func (c Code) String() string {
	return string(c) //strings.Join(Disassemble(c), " ")
}

type Storage map[common.Hash]common.Hash

func (s Storage) String() (str string) {
	for key, value := range s {
		str += fmt.Sprintf("%X : %X\n", key, value)
	}
	return
}

func (s Storage) Copy() Storage {
	cpy := make(Storage, len(s))
	for key, value := range s {
		cpy[key] = value
	}
	return cpy
}

// stateObject represents an Ethereum account which is being modified.
//
// The usage pattern is as follows:
// - First you need to obtain a state object.
// - Account values as well as storages can be accessed and modified through the object.
// - Finally, call commit to return the changes of storage trie and update account data.
type stateObject struct {
	db       *StateDB
	address  common.Address      // address of ethereum account
	addrHash common.Hash         // hash of ethereum address of the account
	origin   *types.StateAccount // Account original data without any change applied, nil means it was not existent
	data     types.StateAccount  // Account data with all mutations applied in the scope of block

	// dirty account state
	dirtyBalance  *uint256.Int
	dirtyNonce    *uint64
	dirtyCodeHash []byte

	// Write caches.
	trie Trie // storage trie, which becomes non-nil on first access
	code Code // contract bytecode, which gets set when code is loaded

	originStorage  Storage // Storage cache of original entries to dedup rewrites
	pendingStorage Storage // Storage entries that need to be flushed to disk, at the end of an entire block
	dirtyStorage   Storage // Storage entries that have been modified in the current transaction execution, reset for every transaction

	// Cache flags.
	dirtyCode bool // true if the code was updated

	// Flag whether the account was marked as self-destructed. The self-destructed account
	// is still accessible in the scope of same transaction.
	selfDestructed bool

	// Flag whether the account was marked as deleted. A self-destructed account
	// or an account that is considered as empty will be marked as deleted at
	// the end of transaction and no longer accessible anymore.
	deleted bool

	// Flag whether the object was created in the current transaction
	created bool
}

// empty returns whether the account is considered empty.
func (s *stateObject) empty() bool {
	return s.Nonce() == 0 && s.Balance().IsZero() && bytes.Equal(s.CodeHash(), types.EmptyCodeHash.Bytes())
}

// newObject creates a state object.
func newObject(db *StateDB, address common.Address, acct *types.StateAccount) *stateObject {
	var (
		origin  = acct
		created = acct == nil // true if the account was not existent
	)
	if acct == nil {
		acct = types.NewEmptyStateAccount()
	}
	s := &stateObject{
		db:             db,
		address:        address,
		addrHash:       crypto.Keccak256Hash(address[:]),
		origin:         origin,
		data:           *acct,
		originStorage:  make(Storage),
		pendingStorage: make(Storage),
		dirtyStorage:   make(Storage),
		created:        created,
	}

	// dirty data when create a new account
	if created {
		s.dirtyBalance = new(uint256.Int).Set(acct.Balance)
		s.dirtyNonce = new(uint64)
		*s.dirtyNonce = acct.Nonce
		s.dirtyCodeHash = acct.CodeHash
	}
	return s
}

// EncodeRLP implements rlp.Encoder.
func (s *stateObject) EncodeRLP(w io.Writer) error {
	return rlp.Encode(w, &s.data)
}

func (s *stateObject) markSelfdestructed() {
	s.selfDestructed = true
}

func (s *stateObject) touch() {
	s.db.journal.append(touchChange{
		account: &s.address,
	})
	if s.address == ripemd {
		// Explicitly put it in the dirty-cache, which is otherwise generated from
		// flattened journals.
		s.db.journal.dirty(s.address)
	}
}

// getTrie returns the associated storage trie. The trie will be opened
// if it's not loaded previously. An error will be returned if trie can't
// be loaded.
func (s *stateObject) getTrie() (Trie, error) {
	if s.trie == nil {
		// Try fetching from prefetcher first
		if s.data.Root != types.EmptyRootHash && s.db.prefetcher != nil {
			// When the miner is creating the pending state, there is no prefetcher
			s.trie = s.db.prefetcher.trie(s.addrHash, s.data.Root)
		}
		if s.trie == nil {
			tr, err := s.db.db.OpenStorageTrie(s.db.originalRoot, s.address, s.data.Root, s.db.trie)
			if err != nil {
				return nil, err
			}
			s.trie = tr
		}
	}
	return s.trie, nil
}

// GetState retrieves a value from the account storage trie.
func (s *stateObject) GetState(key common.Hash) common.Hash {
	// If we have a dirty value for this state entry, return it
	value, dirty := s.dirtyStorage[key]
	if dirty {
		return value
	}
	// Otherwise return the entry's original value
	return s.GetCommittedState(key)
}

// GetCommittedState retrieves a value from the committed account storage trie.
func (s *stateObject) GetCommittedState(key common.Hash) common.Hash {
	// If we have a pending write or clean cached, return that
	if value, pending := s.pendingStorage[key]; pending {
		return value
	}
	if value, cached := s.originStorage[key]; cached {
		return value
	}
	// If the object was destructed in *this* block (and potentially resurrected),
	// the storage has been cleared out, and we should *not* consult the previous
	// database about any storage values. The only possible alternatives are:
	//   1) resurrect happened, and new slot values were set -- those should
	//      have been handles via pendingStorage above.
	//   2) we don't have new values, and can deliver empty response back
	if _, destructed := s.db.getStateObjectsDestruct(s.address); destructed {
		return common.Hash{}
	}
	// If no live objects are available, attempt to use snapshots
	var (
		enc   []byte
		err   error
		value common.Hash
	)
	if s.db.snap != nil {
		start := time.Now()
		enc, err = s.db.snap.Storage(s.addrHash, crypto.Keccak256Hash(key.Bytes()))
		if metrics.EnabledExpensive {
			s.db.SnapshotStorageReads += time.Since(start)
		}
		if len(enc) > 0 {
			_, content, _, err := rlp.Split(enc)
			if err != nil {
				s.db.setError(err)
			}
			value.SetBytes(content)
		}
	}
	// If the snapshot is unavailable or reading from it fails, load from the database.
	if s.db.snap == nil || err != nil {
		start := time.Now()
		tr, err := s.getTrie()
		if err != nil {
			s.db.setError(err)
			return common.Hash{}
		}
		val, err := tr.GetStorage(s.address, key.Bytes())
		if metrics.EnabledExpensive {
			s.db.StorageReads += time.Since(start)
		}
		if err != nil {
			s.db.setError(err)
			return common.Hash{}
		}
		value.SetBytes(val)
	}
	s.originStorage[key] = value
	return value
}

// SetState updates a value in account storage.
func (s *stateObject) SetState(key, value common.Hash) {
	// If the new value is the same as old, don't set
	prev := s.GetState(key)
	if prev == value {
		return
	}
	// New value is different, update and journal the change
	s.db.journal.append(storageChange{
		account:  &s.address,
		key:      key,
		prevalue: prev,
	})
	s.setState(key, value)
}

func (s *stateObject) setState(key, value common.Hash) {
	s.dirtyStorage[key] = value
}

// finalise moves all dirty storage slots into the pending area to be hashed or
// committed later. It is invoked at the end of every transaction.
func (s *stateObject) finalise(prefetch bool) {
	slotsToPrefetch := make([][]byte, 0, len(s.dirtyStorage))
	for key, value := range s.dirtyStorage {
		s.pendingStorage[key] = value
		if value != s.originStorage[key] {
			slotsToPrefetch = append(slotsToPrefetch, common.CopyBytes(key[:])) // Copy needed for closure
		}
	}

	if s.dirtyNonce != nil {
		s.data.Nonce = *s.dirtyNonce
		s.dirtyNonce = nil
	}
	if s.dirtyBalance != nil {
		s.data.Balance = s.dirtyBalance
		s.dirtyBalance = nil
	}
	if s.dirtyCodeHash != nil {
		s.data.CodeHash = s.dirtyCodeHash
		s.dirtyCodeHash = nil
	}
	if s.db.prefetcher != nil && prefetch && len(slotsToPrefetch) > 0 && s.data.Root != types.EmptyRootHash {
		s.db.prefetcher.prefetch(s.addrHash, s.data.Root, s.address, slotsToPrefetch)
	}
	if len(s.dirtyStorage) > 0 {
		s.dirtyStorage = make(Storage)
	}
}

func (s *stateObject) finaliseRWSet() {
	if s.db.mvStates == nil {
		return
	}
	ms := s.db.mvStates
	for key, value := range s.dirtyStorage {
		// three are some unclean dirtyStorage from previous reverted txs, it will skip finalise
		// so add a new rule, if val has no change, then skip it
		if value == s.GetCommittedState(key) {
			continue
		}
		ms.RecordStorageWrite(s.address, key)
	}

	if s.dirtyNonce != nil && *s.dirtyNonce != s.data.Nonce {
		ms.RecordAccountWrite(s.address, types.AccountNonce)
	}
	if s.dirtyBalance != nil && s.dirtyBalance.Cmp(s.data.Balance) != 0 {
		ms.RecordAccountWrite(s.address, types.AccountBalance)
	}
	if s.dirtyCodeHash != nil && !slices.Equal(s.dirtyCodeHash, s.data.CodeHash) {
		ms.RecordAccountWrite(s.address, types.AccountCodeHash)
	}
}

// updateTrie is responsible for persisting cached storage changes into the
// object's storage trie. In case the storage trie is not yet loaded, this
// function will load the trie automatically. If any issues arise during the
// loading or updating of the trie, an error will be returned. Furthermore,
// this function will return the mutated storage trie, or nil if there is no
// storage change at all.
func (s *stateObject) updateTrie() (Trie, error) {
	// Make sure all dirty slots are finalized into the pending storage area
	s.finalise(false)

	// Short circuit if nothing changed, don't bother with hashing anything
	if len(s.pendingStorage) == 0 {
		return s.trie, nil
	}
	// Track the amount of time wasted on updating the storage trie
	if metrics.EnabledExpensive {
		defer func(start time.Time) { s.db.StorageUpdates += time.Since(start) }(time.Now())
	}
	// The snapshot storage map for the object
	var (
		storage map[common.Hash][]byte
		origin  map[common.Hash][]byte
		hasher  = crypto.NewKeccakState()
	)
	tr, err := s.getTrie()
	if err != nil {
		s.db.setError(err)
		return nil, err
	}
	// Insert all the pending storage updates into the trie
	usedStorage := make([][]byte, 0, len(s.pendingStorage))
	dirtyStorage := make(map[common.Hash][]byte)

	for key, value := range s.pendingStorage {
		// Skip noop changes, persist actual changes
		if value == s.originStorage[key] {
			continue
		}
		var v []byte
		if value != (common.Hash{}) {
			value := value
			v = common.TrimLeftZeroes(value[:])
		}
		dirtyStorage[key] = v
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for key, value := range dirtyStorage {
			if len(value) == 0 {
				if err := tr.DeleteStorage(s.address, key[:]); err != nil {
					s.db.setError(err)
				}
				s.db.StorageDeleted += 1
			} else {
				if err := tr.UpdateStorage(s.address, key[:], value); err != nil {
					s.db.setError(err)
				}
				s.db.StorageUpdated += 1
			}
			// Cache the items for preloading
			usedStorage = append(usedStorage, common.CopyBytes(key[:]))
		}
	}()
	// If state snapshotting is active, cache the data til commit
	wg.Add(1)
	go func() {
		defer wg.Done()
		s.db.StorageMux.Lock()
		// The snapshot storage map for the object
		storage = s.db.storages[s.addrHash]
		if storage == nil {
			storage = make(map[common.Hash][]byte, len(dirtyStorage))
			s.db.storages[s.addrHash] = storage
		}
		// Cache the original value of mutated storage slots
		origin = s.db.storagesOrigin[s.address]
		if origin == nil {
			origin = make(map[common.Hash][]byte)
			s.db.storagesOrigin[s.address] = origin
		}
		s.db.StorageMux.Unlock()
		for key, value := range dirtyStorage {
			khash := crypto.HashData(hasher, key[:])

			// rlp-encoded value to be used by the snapshot
			var encoded []byte
			if len(value) != 0 {
				encoded, _ = rlp.EncodeToBytes(value)
			}
			storage[khash] = encoded // encoded will be nil if it's deleted

			// Track the original value of slot only if it's mutated first time
			prev := s.originStorage[key]
			s.originStorage[key] = common.BytesToHash(value) // fill back left zeroes by BytesToHash
			if _, ok := origin[khash]; !ok {
				if prev == (common.Hash{}) {
					origin[khash] = nil // nil if it was not present previously
				} else {
					// Encoding []byte cannot fail, ok to ignore the error.
					b, _ := rlp.EncodeToBytes(common.TrimLeftZeroes(prev[:]))
					origin[khash] = b
				}
			}
		}
	}()
	wg.Wait()

	if s.db.prefetcher != nil {
		s.db.prefetcher.used(s.addrHash, s.data.Root, usedStorage)
	}
	s.pendingStorage = make(Storage) // reset pending map
	return tr, nil
}

// updateRoot flushes all cached storage mutations to trie, recalculating the
// new storage trie root.
func (s *stateObject) updateRoot() {
	// If node runs in no trie mode, set root to empty.
	defer func() {
		if s.db.db.NoTries() {
			s.data.Root = types.EmptyRootHash
		}
	}()

	// Flush cached storage mutations into trie, short circuit if any error
	// is occurred or there is not change in the trie.
	tr, err := s.updateTrie()
	if err != nil || tr == nil {
		return
	}
	// Track the amount of time wasted on hashing the storage trie
	if metrics.EnabledExpensive {
		defer func(start time.Time) { s.db.StorageHashes += time.Since(start) }(time.Now())
	}
	s.data.Root = tr.Hash()
}

// commit obtains a set of dirty storage trie nodes and updates the account data.
// The returned set can be nil if nothing to commit. This function assumes all
// storage mutations have already been flushed into trie by updateRoot.
func (s *stateObject) commit() (*trienode.NodeSet, error) {
	// Short circuit if trie is not even loaded, don't bother with committing anything
	if s.trie == nil {
		s.origin = s.data.Copy()
		return nil, nil
	}
	// Track the amount of time wasted on committing the storage trie
	if metrics.EnabledExpensive {
		defer func(start time.Time) { s.db.StorageCommits += time.Since(start) }(time.Now())
	}
	// The trie is currently in an open state and could potentially contain
	// cached mutations. Call commit to acquire a set of nodes that have been
	// modified, the set can be nil if nothing to commit.
	root, nodes, err := s.trie.Commit(false)
	if err != nil {
		return nil, err
	}
	s.data.Root = root

	// Update original account data after commit
	s.origin = s.data.Copy()
	return nodes, nil
}

// AddBalance adds amount to s's balance.
// It is used to add funds to the destination account of a transfer.
func (s *stateObject) AddBalance(amount *uint256.Int) {
	// EIP161: We must check emptiness for the objects such that the account
	// clearing (0,0,0 objects) can take effect.
	if amount.IsZero() {
		if s.empty() {
			s.touch()
		}
		return
	}
	s.SetBalance(new(uint256.Int).Add(s.Balance(), amount))
}

// SubBalance removes amount from s's balance.
// It is used to remove funds from the origin account of a transfer.
func (s *stateObject) SubBalance(amount *uint256.Int) {
	if amount.IsZero() {
		return
	}
	s.SetBalance(new(uint256.Int).Sub(s.Balance(), amount))
}

func (s *stateObject) SetBalance(amount *uint256.Int) {
	s.db.journal.append(balanceChange{
		account: &s.address,
		prev:    new(uint256.Int).Set(s.Balance()),
	})
	s.setBalance(amount)
}

func (s *stateObject) setBalance(amount *uint256.Int) {
	s.dirtyBalance = amount
}

func (s *stateObject) deepCopy(db *StateDB) *stateObject {
	obj := &stateObject{
		db:       db,
		address:  s.address,
		addrHash: s.addrHash,
		origin:   s.origin,
		data:     s.data,
	}
	if s.trie != nil {
		obj.trie = db.db.CopyTrie(s.trie)
	}
	obj.code = s.code
	obj.dirtyStorage = s.dirtyStorage.Copy()
	obj.originStorage = s.originStorage.Copy()
	obj.pendingStorage = s.pendingStorage.Copy()
	obj.selfDestructed = s.selfDestructed
	obj.dirtyCode = s.dirtyCode
	obj.deleted = s.deleted
	if s.dirtyBalance != nil {
		obj.dirtyBalance = new(uint256.Int).Set(s.dirtyBalance)
	}
	if s.dirtyNonce != nil {
		obj.dirtyNonce = new(uint64)
		*obj.dirtyNonce = *s.dirtyNonce
	}
	if s.dirtyCodeHash != nil {
		obj.dirtyCodeHash = s.dirtyCodeHash
	}
	return obj
}

//
// Attribute accessors
//

// Address returns the address of the contract/account
func (s *stateObject) Address() common.Address {
	return s.address
}

// Code returns the contract code associated with this object, if any.
func (s *stateObject) Code() []byte {
	if s.code != nil {
		return s.code
	}
	if bytes.Equal(s.CodeHash(), types.EmptyCodeHash.Bytes()) {
		return nil
	}
	code, err := s.db.db.ContractCode(s.address, common.BytesToHash(s.CodeHash()))
	if err != nil {
		s.db.setError(fmt.Errorf("can't load code hash %x: %v", s.CodeHash(), err))
	}
	s.code = code
	return code
}

// CodeSize returns the size of the contract code associated with this object,
// or zero if none. This method is an almost mirror of Code, but uses a cache
// inside the database to avoid loading codes seen recently.
func (s *stateObject) CodeSize() int {
	if s.code != nil {
		return len(s.code)
	}
	if bytes.Equal(s.CodeHash(), types.EmptyCodeHash.Bytes()) {
		return 0
	}
	size, err := s.db.db.ContractCodeSize(s.address, common.BytesToHash(s.CodeHash()))
	if err != nil {
		s.db.setError(fmt.Errorf("can't load code size %x: %v", s.CodeHash(), err))
	}
	return size
}

func (s *stateObject) SetCode(codeHash common.Hash, code []byte) {
	prevcode := s.Code()
	s.db.journal.append(codeChange{
		account:  &s.address,
		prevhash: s.CodeHash(),
		prevcode: prevcode,
	})
	s.setCode(codeHash, code)
}

func (s *stateObject) setCode(codeHash common.Hash, code []byte) {
	s.code = code
	s.dirtyCodeHash = codeHash[:]
	s.dirtyCode = true
	compiler.GenOrLoadOptimizedCode(codeHash, s.code)
}

func (s *stateObject) SetNonce(nonce uint64) {
	s.db.journal.append(nonceChange{
		account: &s.address,
		prev:    s.Nonce(),
	})
	s.setNonce(nonce)
}

func (s *stateObject) setNonce(nonce uint64) {
	s.dirtyNonce = &nonce
}

func (s *stateObject) CodeHash() []byte {
	if len(s.dirtyCodeHash) > 0 {
		return s.dirtyCodeHash
	}
	return s.data.CodeHash
}

func (s *stateObject) Balance() *uint256.Int {
	if s.dirtyBalance != nil {
		return s.dirtyBalance
	}
	return s.data.Balance
}

func (s *stateObject) Nonce() uint64 {
	if s.dirtyNonce != nil {
		return *s.dirtyNonce
	}
	return s.data.Nonce
}

func (s *stateObject) Root() common.Hash {
	return s.data.Root
}
