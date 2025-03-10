package iavl

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"runtime"
	"sort"
	"sync"
	"time"

	"github.com/pkg/errors"

	dbm "github.com/tendermint/tm-db"
)

// ErrVersionDoesNotExist is returned if a requested version does not exist.
var ErrVersionDoesNotExist = errors.New("version does not exist")

// MutableTree is a persistent tree which keeps track of versions. It is not safe for concurrent
// use, and should be guarded by a Mutex or RWLock as appropriate. An immutable tree at a given
// version can be returned via GetImmutable, which is safe for concurrent access.
//
// Given and returned key/value byte slices must not be modified, since they may point to data
// located inside IAVL which would also be modified.
//
// The inner ImmutableTree should not be used directly by callers.
type MutableTree struct {
	*ImmutableTree                                  // The current, working tree.
	lastSaved                *ImmutableTree         // The most recently saved tree.
	orphans                  map[string]int64       // Nodes removed by changes to working tree.
	versions                 map[int64]bool         // The previous, saved versions of the tree.
	allRootLoaded            bool                   // Whether all roots are loaded or not(by LazyLoadVersion)
	unsavedFastNodeAdditions map[string]*FastNode   // FastNodes that have not yet been saved to disk
	unsavedFastNodeRemovals  map[string]interface{} // FastNodes that have not yet been removed from disk
	ndb                      *nodeDB

	mtx sync.Mutex
}

// NewMutableTree returns a new tree with the specified cache size and datastore.
func NewMutableTree(db dbm.DB, cacheSize int) (*MutableTree, error) {
	return NewMutableTreeWithOpts(db, cacheSize, nil)
}

// NewMutableTreeWithOpts returns a new tree with the specified options.
func NewMutableTreeWithOpts(db dbm.DB, cacheSize int, opts *Options) (*MutableTree, error) {
	ndb := newNodeDB(db, cacheSize, opts)
	head := &ImmutableTree{ndb: ndb}

	return &MutableTree{
		ImmutableTree:            head,
		lastSaved:                head.clone(),
		orphans:                  map[string]int64{},
		versions:                 map[int64]bool{},
		allRootLoaded:            false,
		unsavedFastNodeAdditions: make(map[string]*FastNode),
		unsavedFastNodeRemovals:  make(map[string]interface{}),
		ndb:                      ndb,
	}, nil
}

// IsEmpty returns whether or not the tree has any keys. Only trees that are
// not empty can be saved.
func (tree *MutableTree) IsEmpty() bool {
	return tree.ImmutableTree.Size() == 0
}

// VersionExists returns whether or not a version exists.
func (tree *MutableTree) VersionExists(version int64) bool {
	tree.mtx.Lock()
	defer tree.mtx.Unlock()

	if tree.allRootLoaded {
		return tree.versions[version]
	}

	has, ok := tree.versions[version]
	if ok {
		return has
	}
	has, _ = tree.ndb.HasRoot(version)
	tree.versions[version] = has
	return has
}

// AvailableVersions returns all available versions in ascending order
func (tree *MutableTree) AvailableVersions() []int {
	tree.mtx.Lock()
	defer tree.mtx.Unlock()

	res := make([]int, 0, len(tree.versions))
	for i, v := range tree.versions {
		if v {
			res = append(res, int(i))
		}
	}
	sort.Ints(res)
	return res
}

// Hash returns the hash of the latest saved version of the tree, as returned
// by SaveVersion. If no versions have been saved, Hash returns nil.
func (tree *MutableTree) Hash() []byte {
	return tree.lastSaved.Hash()
}

// WorkingHash returns the hash of the current working tree.
func (tree *MutableTree) WorkingHash() []byte {
	return tree.ImmutableTree.Hash()
}

// String returns a string representation of the tree.
func (tree *MutableTree) String() (string, error) {
	return tree.ndb.String()
}

// Set/Remove will orphan at most tree.Height nodes,
// balancing the tree after a Set/Remove will orphan at most 3 nodes.
func (tree *MutableTree) prepareOrphansSlice() []*Node {
	return make([]*Node, 0, tree.Height()+3)
}

// Set sets a key in the working tree. Nil values are invalid. The given
// key/value byte slices must not be modified after this call, since they point
// to slices stored within IAVL. It returns true when an existing value was
// updated, while false means it was a new key.
func (tree *MutableTree) Set(key, value []byte) (updated bool) {
	var orphaned []*Node
	orphaned, updated = tree.set(key, value)
	tree.addOrphans(orphaned)
	return updated
}

// Get returns the value of the specified key if it exists, or nil otherwise.
// The returned value must not be modified, since it may point to data stored within IAVL.
func (t *MutableTree) Get(key []byte) []byte {
	if t.root == nil {
		return nil
	}

	if fastNode, ok := t.unsavedFastNodeAdditions[string(key)]; ok {
		return fastNode.value
	}

	return t.ImmutableTree.Get(key)
}

// Import returns an importer for tree nodes previously exported by ImmutableTree.Export(),
// producing an identical IAVL tree. The caller must call Close() on the importer when done.
//
// version should correspond to the version that was initially exported. It must be greater than
// or equal to the highest ExportNode version number given.
//
// Import can only be called on an empty tree. It is the callers responsibility that no other
// modifications are made to the tree while importing.
func (tree *MutableTree) Import(version int64) (*Importer, error) {
	return newImporter(tree, version)
}

// Iterate iterates over all keys of the tree. The keys and values must not be modified,
// since they may point to data stored within IAVL. Returns true if stopped by callnack, false otherwise
func (t *MutableTree) Iterate(fn func(key []byte, value []byte) bool) (stopped bool) {
	if t.root == nil {
		return false
	}

	if !t.IsFastCacheEnabled() {
		return t.ImmutableTree.Iterate(fn)
	}

	itr := NewUnsavedFastIterator(nil, nil, true, t.ndb, t.unsavedFastNodeAdditions, t.unsavedFastNodeRemovals)
	defer itr.Close()

	for ; itr.Valid(); itr.Next() {
		if fn(itr.Key(), itr.Value()) {
			return true
		}
	}

	return false
}

// Iterator returns an iterator over the mutable tree.
// CONTRACT: no updates are made to the tree while an iterator is active.
func (t *MutableTree) Iterator(start, end []byte, ascending bool) dbm.Iterator {
	if t.IsFastCacheEnabled() {
		return NewUnsavedFastIterator(start, end, ascending, t.ndb, t.unsavedFastNodeAdditions, t.unsavedFastNodeRemovals)
	}
	return t.ImmutableTree.Iterator(start, end, ascending)
}

func (tree *MutableTree) set(key []byte, value []byte) (orphans []*Node, updated bool) {
	if value == nil {
		panic(fmt.Sprintf("Attempt to store nil value at key '%s'", key))
	}

	if tree.ImmutableTree.root == nil {
		tree.addUnsavedAddition(key, NewFastNode(key, value, tree.version+1))
		tree.ImmutableTree.root = NewNode(key, value, tree.version+1)
		return nil, updated
	}

	orphans = tree.prepareOrphansSlice()
	tree.ImmutableTree.root, updated = tree.recursiveSet(tree.ImmutableTree.root, key, value, &orphans)
	return orphans, updated
}

func (tree *MutableTree) recursiveSet(node *Node, key []byte, value []byte, orphans *[]*Node) (
	newSelf *Node, updated bool,
) {
	version := tree.version + 1

	if node.isLeaf() {
		tree.addUnsavedAddition(key, NewFastNode(key, value, version))

		switch bytes.Compare(key, node.key) {
		case -1:
			return &Node{
				key:       node.key,
				height:    1,
				size:      2,
				leftNode:  NewNode(key, value, version),
				rightNode: node,
				version:   version,
			}, false
		case 1:
			return &Node{
				key:       key,
				height:    1,
				size:      2,
				leftNode:  node,
				rightNode: NewNode(key, value, version),
				version:   version,
			}, false
		default:
			*orphans = append(*orphans, node)
			return NewNode(key, value, version), true
		}
	} else {
		*orphans = append(*orphans, node)
		node = node.clone(version)

		if bytes.Compare(key, node.key) < 0 {
			node.leftNode, updated = tree.recursiveSet(node.getLeftNode(tree.ImmutableTree), key, value, orphans)
			node.leftHash = nil // leftHash is yet unknown
		} else {
			node.rightNode, updated = tree.recursiveSet(node.getRightNode(tree.ImmutableTree), key, value, orphans)
			node.rightHash = nil // rightHash is yet unknown
		}

		if updated {
			return node, updated
		}
		node.calcHeightAndSize(tree.ImmutableTree)
		newNode := tree.balance(node, orphans)
		return newNode, updated
	}
}

// Remove removes a key from the working tree. The given key byte slice should not be modified
// after this call, since it may point to data stored inside IAVL.
func (tree *MutableTree) Remove(key []byte) ([]byte, bool) {
	val, orphaned, removed := tree.remove(key)
	tree.addOrphans(orphaned)
	return val, removed
}

// remove tries to remove a key from the tree and if removed, returns its
// value, nodes orphaned and 'true'.
func (tree *MutableTree) remove(key []byte) (value []byte, orphaned []*Node, removed bool) {
	if tree.root == nil {
		return nil, nil, false
	}
	orphaned = tree.prepareOrphansSlice()
	newRootHash, newRoot, _, value := tree.recursiveRemove(tree.root, key, &orphaned)
	if len(orphaned) == 0 {
		return nil, nil, false
	}

	tree.addUnsavedRemoval(key)

	if newRoot == nil && newRootHash != nil {
		tree.root = tree.ndb.GetNode(newRootHash)
	} else {
		tree.root = newRoot
	}
	return value, orphaned, true
}

// removes the node corresponding to the passed key and balances the tree.
// It returns:
// - the hash of the new node (or nil if the node is the one removed)
// - the node that replaces the orig. node after remove
// - new leftmost leaf key for tree after successfully removing 'key' if changed.
// - the removed value
// - the orphaned nodes.
func (tree *MutableTree) recursiveRemove(node *Node, key []byte, orphans *[]*Node) (newHash []byte, newSelf *Node, newKey []byte, newValue []byte) {
	version := tree.version + 1

	if node.isLeaf() {
		if bytes.Equal(key, node.key) {
			*orphans = append(*orphans, node)
			return nil, nil, nil, node.value
		}
		return node.hash, node, nil, nil
	}

	// node.key < key; we go to the left to find the key:
	if bytes.Compare(key, node.key) < 0 {
		newLeftHash, newLeftNode, newKey, value := tree.recursiveRemove(node.getLeftNode(tree.ImmutableTree), key, orphans) //nolint:govet

		if len(*orphans) == 0 {
			return node.hash, node, nil, value
		}
		*orphans = append(*orphans, node)
		if newLeftHash == nil && newLeftNode == nil { // left node held value, was removed
			return node.rightHash, node.rightNode, node.key, value
		}

		newNode := node.clone(version)
		newNode.leftHash, newNode.leftNode = newLeftHash, newLeftNode
		newNode.calcHeightAndSize(tree.ImmutableTree)
		newNode = tree.balance(newNode, orphans)
		return newNode.hash, newNode, newKey, value
	}
	// node.key >= key; either found or look to the right:
	newRightHash, newRightNode, newKey, value := tree.recursiveRemove(node.getRightNode(tree.ImmutableTree), key, orphans)

	if len(*orphans) == 0 {
		return node.hash, node, nil, value
	}
	*orphans = append(*orphans, node)
	if newRightHash == nil && newRightNode == nil { // right node held value, was removed
		return node.leftHash, node.leftNode, nil, value
	}

	newNode := node.clone(version)
	newNode.rightHash, newNode.rightNode = newRightHash, newRightNode
	if newKey != nil {
		newNode.key = newKey
	}
	newNode.calcHeightAndSize(tree.ImmutableTree)
	newNode = tree.balance(newNode, orphans)
	return newNode.hash, newNode, nil, value
}

// Load the latest versioned tree from disk.
func (tree *MutableTree) Load() (int64, error) {
	return tree.LoadVersion(int64(0))
}

// LazyLoadVersion attempts to lazy load only the specified target version
// without loading previous roots/versions. Lazy loading should be used in cases
// where only reads are expected. Any writes to a lazy loaded tree may result in
// unexpected behavior. If the targetVersion is non-positive, the latest version
// will be loaded by default. If the latest version is non-positive, this method
// performs a no-op. Otherwise, if the root does not exist, an error will be
// returned.
func (tree *MutableTree) LazyLoadVersion(targetVersion int64) (int64, error) {
	latestVersion := tree.ndb.getLatestVersion()
	if latestVersion < targetVersion {
		return latestVersion, fmt.Errorf("wanted to load target %d but only found up to %d", targetVersion, latestVersion)
	}

	// no versions have been saved if the latest version is non-positive
	if latestVersion <= 0 {
		if targetVersion <= 0 {
			tree.mtx.Lock()
			defer tree.mtx.Unlock()
			_, err := tree.enableFastStorageAndCommitIfNotEnabled()
			return 0, err
		}
		return 0, fmt.Errorf("no versions found while trying to load %v", targetVersion)
	}

	// default to the latest version if the targeted version is non-positive
	if targetVersion <= 0 {
		targetVersion = latestVersion
	}

	rootHash, err := tree.ndb.getRoot(targetVersion)
	if err != nil {
		return 0, err
	}
	if rootHash == nil {
		return latestVersion, ErrVersionDoesNotExist
	}

	tree.mtx.Lock()
	defer tree.mtx.Unlock()

	tree.versions[targetVersion] = true

	iTree := &ImmutableTree{
		ndb:     tree.ndb,
		version: targetVersion,
	}
	if len(rootHash) > 0 {
		// If rootHash is empty then root of tree should be nil
		// This makes `LazyLoadVersion` to do the same thing as `LoadVersion`
		iTree.root = tree.ndb.GetNode(rootHash)
	}

	tree.orphans = map[string]int64{}
	tree.ImmutableTree = iTree
	tree.lastSaved = iTree.clone()

	// Attempt to upgrade
	if _, err := tree.enableFastStorageAndCommitIfNotEnabled(); err != nil {
		return 0, err
	}

	return targetVersion, nil
}

// Returns the version number of the latest version found
func (tree *MutableTree) LoadVersion(targetVersion int64) (int64, error) {
	roots, err := tree.ndb.getRoots()
	if err != nil {
		return 0, err
	}

	if len(roots) == 0 {
		if targetVersion <= 0 {
			tree.mtx.Lock()
			defer tree.mtx.Unlock()
			_, err := tree.enableFastStorageAndCommitIfNotEnabled()
			return 0, err
		}
		return 0, fmt.Errorf("no versions found while trying to load %v", targetVersion)
	}

	firstVersion := int64(0)
	latestVersion := int64(0)

	tree.mtx.Lock()
	defer tree.mtx.Unlock()

	var latestRoot []byte
	for version, r := range roots {
		tree.versions[version] = true
		if version > latestVersion && (targetVersion == 0 || version <= targetVersion) {
			latestVersion = version
			latestRoot = r
		}
		if firstVersion == 0 || version < firstVersion {
			firstVersion = version
		}
	}

	if !(targetVersion == 0 || latestVersion == targetVersion) {
		return latestVersion, fmt.Errorf("wanted to load target %v but only found up to %v",
			targetVersion, latestVersion)
	}

	if firstVersion > 0 && firstVersion < int64(tree.ndb.opts.InitialVersion) {
		return latestVersion, fmt.Errorf("initial version set to %v, but found earlier version %v",
			tree.ndb.opts.InitialVersion, firstVersion)
	}

	t := &ImmutableTree{
		ndb:     tree.ndb,
		version: latestVersion,
	}

	if len(latestRoot) != 0 {
		t.root = tree.ndb.GetNode(latestRoot)
	}

	tree.orphans = map[string]int64{}
	tree.ImmutableTree = t
	tree.lastSaved = t.clone()
	tree.allRootLoaded = true

	// Attempt to upgrade
	if _, err := tree.enableFastStorageAndCommitIfNotEnabled(); err != nil {
		return 0, err
	}

	return latestVersion, nil
}

// LoadVersionForOverwriting attempts to load a tree at a previously committed
// version, or the latest version below it. Any versions greater than targetVersion will be deleted.
func (tree *MutableTree) LoadVersionForOverwriting(targetVersion int64) (int64, error) {
	latestVersion, err := tree.LoadVersion(targetVersion)
	if err != nil {
		return latestVersion, err
	}

	if err = tree.ndb.DeleteVersionsFrom(targetVersion + 1); err != nil {
		return latestVersion, err
	}

	if err := tree.enableFastStorageAndCommitLocked(); err != nil {
		return latestVersion, err
	}

	tree.ndb.resetLatestVersion(latestVersion)

	tree.mtx.Lock()
	defer tree.mtx.Unlock()

	for v := range tree.versions {
		if v > targetVersion {
			delete(tree.versions, v)
		}
	}

	return latestVersion, nil
}

// Returns true if the tree may be auto-upgraded, false otherwise
// An example of when an upgrade may be performed is when we are enaling fast storage for the first time or
// need to overwrite fast nodes due to mismatch with live state.
func (tree *MutableTree) IsUpgradeable() bool {
	return !tree.ndb.hasUpgradedToFastStorage() || tree.ndb.shouldForceFastStorageUpgrade()
}

// enableFastStorageAndCommitIfNotEnabled if nodeDB doesn't mark fast storage as enabled, enable it, and commit the update.
// Checks whether the fast cache on disk matches latest live state. If not, deletes all existing fast nodes and repopulates them
// from latest tree.
func (tree *MutableTree) enableFastStorageAndCommitIfNotEnabled() (bool, error) {
	shouldForceUpdate := tree.ndb.shouldForceFastStorageUpgrade()
	isFastStorageEnabled := tree.ndb.hasUpgradedToFastStorage()

	if !tree.IsUpgradeable() {
		return false, nil
	}

	if isFastStorageEnabled && shouldForceUpdate {
		// If there is a mismatch between which fast nodes are on disk and the live state due to temporary
		// downgrade and subsequent re-upgrade, we cannot know for sure which fast nodes have been removed while downgraded,
		// Therefore, there might exist stale fast nodes on disk. As a result, to avoid persisting the stale state, it might
		// be worth to delete the fast nodes from disk.
		fastItr := NewFastIterator(nil, nil, true, tree.ndb)
		defer fastItr.Close()
		for ; fastItr.Valid(); fastItr.Next() {
			if err := tree.ndb.DeleteFastNode(fastItr.Key()); err != nil {
				return false, err
			}
		}
	}

	// Force garbage collection before we proceed to enabling fast storage.
	runtime.GC()

	if err := tree.enableFastStorageAndCommit(); err != nil {
		tree.ndb.storageVersion = defaultStorageVersionValue
		return false, err
	}
	return true, nil
}

func (tree *MutableTree) enableFastStorageAndCommitLocked() error {
	tree.mtx.Lock()
	defer tree.mtx.Unlock()
	return tree.enableFastStorageAndCommit()
}

func (tree *MutableTree) enableFastStorageAndCommit() error {
	debug("enabling fast storage, might take a while.")
	var err error
	defer func() {
		if err != nil {
			debug("failed to enable fast storage: %v\n", err)
		} else {
			debug("fast storage is enabled.")
		}
	}()

	// We start a new thread to keep on checking if we are above 4GB, and if so garbage collect.
	// This thread only lasts during the fast node migration.
	// This is done to keep RAM usage down.
	done := make(chan struct{})
	defer func() {
		done <- struct{}{}
		close(done)
	}()

	go func() {
		timer := time.NewTimer(time.Second)
		var m runtime.MemStats

		for {
			// Sample the current memory usage
			runtime.ReadMemStats(&m)

			if m.Alloc > 4*1024*1024*1024 {
				// If we are using more than 4GB of memory, we should trigger garbage collection
				// to free up some memory.
				runtime.GC()
			}

			select {
			case <-timer.C:
				timer.Reset(time.Second)
			case <-done:
				if !timer.Stop() {
					<-timer.C
				}
				return
			}
		}
	}()

	itr := NewIterator(nil, nil, true, tree.ImmutableTree)
	defer itr.Close()
	for ; itr.Valid(); itr.Next() {
		if err = tree.ndb.SaveFastNodeNoCache(NewFastNode(itr.Key(), itr.Value(), tree.version)); err != nil {
			return err
		}
	}

	if err = itr.Error(); err != nil {
		return err
	}

	if err = tree.ndb.setFastStorageVersionToBatch(); err != nil {
		return err
	}

	if err = tree.ndb.Commit(); err != nil {
		return err
	}
	return nil
}

// GetImmutable loads an ImmutableTree at a given version for querying. The returned tree is
// safe for concurrent access, provided the version is not deleted, e.g. via `DeleteVersion()`.
func (tree *MutableTree) GetImmutable(version int64) (*ImmutableTree, error) {
	rootHash, err := tree.ndb.getRoot(version)
	if err != nil {
		return nil, err
	}
	if rootHash == nil {
		return nil, ErrVersionDoesNotExist
	}

	tree.mtx.Lock()
	defer tree.mtx.Unlock()
	if len(rootHash) == 0 {
		tree.versions[version] = true
		return &ImmutableTree{
			ndb:     tree.ndb,
			version: version,
		}, nil
	}
	tree.versions[version] = true
	return &ImmutableTree{
		root:    tree.ndb.GetNode(rootHash),
		ndb:     tree.ndb,
		version: version,
	}, nil
}

// Rollback resets the working tree to the latest saved version, discarding
// any unsaved modifications.
func (tree *MutableTree) Rollback() {
	if tree.version > 0 {
		tree.ImmutableTree = tree.lastSaved.clone()
	} else {
		tree.ImmutableTree = &ImmutableTree{ndb: tree.ndb, version: 0}
	}
	tree.orphans = map[string]int64{}
	tree.unsavedFastNodeAdditions = map[string]*FastNode{}
	tree.unsavedFastNodeRemovals = map[string]interface{}{}
}

// GetVersioned gets the value at the specified key and version. The returned value must not be
// modified, since it may point to data stored within IAVL.
func (tree *MutableTree) GetVersioned(key []byte, version int64) []byte {
	if tree.VersionExists(version) {
		if tree.IsFastCacheEnabled() {
			fastNode, _ := tree.ndb.GetFastNode(key)
			if fastNode == nil && version == tree.ndb.latestVersion {
				return nil
			}

			if fastNode != nil && fastNode.versionLastUpdatedAt <= version {
				return fastNode.value
			}
		}
		t, err := tree.GetImmutable(version)
		if err != nil {
			return nil
		}
		value := t.Get(key)
		return value
	}
	return nil
}

// SaveVersion saves a new tree version to disk, based on the current state of
// the tree. Returns the hash and new version number.
func (tree *MutableTree) SaveVersion() ([]byte, int64, error) {
	version := tree.version + 1
	if version == 1 && tree.ndb.opts.InitialVersion > 0 {
		version = int64(tree.ndb.opts.InitialVersion)
	}

	if tree.VersionExists(version) {
		// If the version already exists, return an error as we're attempting to overwrite.
		// However, the same hash means idempotent (i.e. no-op).
		existingHash, err := tree.ndb.getRoot(version)
		if err != nil {
			return nil, version, err
		}

		// If the existing root hash is empty (because the tree is empty), then we need to
		// compare with the hash of an empty input which is what `WorkingHash()` returns.
		if len(existingHash) == 0 {
			existingHash = sha256.New().Sum(nil)
		}

		var newHash = tree.WorkingHash()

		if bytes.Equal(existingHash, newHash) {
			tree.version = version
			tree.ImmutableTree = tree.ImmutableTree.clone()
			tree.lastSaved = tree.ImmutableTree.clone()
			tree.orphans = map[string]int64{}
			return existingHash, version, nil
		}

		return nil, version, fmt.Errorf("version %d was already saved to different hash %X (existing hash %X)", version, newHash, existingHash)
	}

	if tree.root == nil {
		// There can still be orphans, for example if the root is the node being
		// removed.
		debug("SAVE EMPTY TREE %v\n", version)
		tree.ndb.SaveOrphans(version, tree.orphans)
		if err := tree.ndb.SaveEmptyRoot(version); err != nil {
			return nil, 0, err
		}
	} else {
		debug("SAVE TREE %v\n", version)
		tree.ndb.SaveBranch(tree.root)
		tree.ndb.SaveOrphans(version, tree.orphans)
		if err := tree.ndb.SaveRoot(tree.root, version); err != nil {
			return nil, 0, err
		}
	}

	if err := tree.saveFastNodeVersion(); err != nil {
		return nil, version, err
	}

	if err := tree.ndb.Commit(); err != nil {
		return nil, version, err
	}

	tree.mtx.Lock()
	defer tree.mtx.Unlock()
	tree.version = version
	tree.versions[version] = true

	// set new working tree
	tree.ImmutableTree = tree.ImmutableTree.clone()
	tree.lastSaved = tree.ImmutableTree.clone()
	tree.orphans = map[string]int64{}
	tree.unsavedFastNodeAdditions = make(map[string]*FastNode)
	tree.unsavedFastNodeRemovals = make(map[string]interface{})

	return tree.Hash(), version, nil
}

func (tree *MutableTree) saveFastNodeVersion() error {
	if err := tree.saveFastNodeAdditions(); err != nil {
		return err
	}
	if err := tree.saveFastNodeRemovals(); err != nil {
		return err
	}

	if err := tree.ndb.setFastStorageVersionToBatch(); err != nil {
		return err
	}

	return nil
}

func (tree *MutableTree) getUnsavedFastNodeAdditions() map[string]*FastNode {
	return tree.unsavedFastNodeAdditions
}

// getUnsavedFastNodeRemovals returns unsaved FastNodes to remove
func (tree *MutableTree) getUnsavedFastNodeRemovals() map[string]interface{} {
	return tree.unsavedFastNodeRemovals
}

func (tree *MutableTree) addUnsavedAddition(key []byte, node *FastNode) {
	delete(tree.unsavedFastNodeRemovals, string(key))
	tree.unsavedFastNodeAdditions[string(key)] = node
}

func (tree *MutableTree) saveFastNodeAdditions() error {
	keysToSort := make([]string, 0, len(tree.unsavedFastNodeAdditions))
	for key := range tree.unsavedFastNodeAdditions {
		keysToSort = append(keysToSort, key)
	}
	sort.Strings(keysToSort)

	for _, key := range keysToSort {
		if err := tree.ndb.SaveFastNode(tree.unsavedFastNodeAdditions[key]); err != nil {
			return err
		}
	}
	return nil
}

func (tree *MutableTree) addUnsavedRemoval(key []byte) {
	delete(tree.unsavedFastNodeAdditions, string(key))
	tree.unsavedFastNodeRemovals[string(key)] = true
}

func (tree *MutableTree) saveFastNodeRemovals() error {
	keysToSort := make([]string, 0, len(tree.unsavedFastNodeRemovals))
	for key := range tree.unsavedFastNodeRemovals {
		keysToSort = append(keysToSort, key)
	}
	sort.Strings(keysToSort)

	for _, key := range keysToSort {
		tree.ndb.DeleteFastNode([]byte(key))
	}
	return nil
}

func (tree *MutableTree) deleteVersion(version int64) error {
	if version <= 0 {
		return errors.New("version must be greater than 0")
	}
	if version == tree.version {
		return errors.Errorf("cannot delete latest saved version (%d)", version)
	}
	if !tree.VersionExists(version) {
		return errors.Wrap(ErrVersionDoesNotExist, "")
	}
	if err := tree.ndb.DeleteVersion(version, true); err != nil {
		return err
	}

	return nil
}

// SetInitialVersion sets the initial version of the tree, replacing Options.InitialVersion.
// It is only used during the initial SaveVersion() call for a tree with no other versions,
// and is otherwise ignored.
func (tree *MutableTree) SetInitialVersion(version uint64) {
	tree.ndb.opts.InitialVersion = version
}

// DeleteVersions deletes a series of versions from the MutableTree.
// Deprecated: please use DeleteVersionsRange instead.
func (tree *MutableTree) DeleteVersions(versions ...int64) error {
	debug("DELETING VERSIONS: %v\n", versions)

	if len(versions) == 0 {
		return nil
	}

	sort.Slice(versions, func(i, j int) bool {
		return versions[i] < versions[j]
	})

	// Find ordered data and delete by interval
	intervals := map[int64]int64{}
	var fromVersion int64
	for _, version := range versions {
		if version-fromVersion != intervals[fromVersion] {
			fromVersion = version
		}
		intervals[fromVersion]++
	}

	for fromVersion, sortedBatchSize := range intervals {
		if err := tree.DeleteVersionsRange(fromVersion, fromVersion+sortedBatchSize); err != nil {
			return err
		}
	}

	return nil
}

// DeleteVersionsRange removes versions from an interval from the MutableTree (not inclusive).
// An error is returned if any single version has active readers.
// All writes happen in a single batch with a single commit.
func (tree *MutableTree) DeleteVersionsRange(fromVersion, toVersion int64) error {
	if err := tree.ndb.DeleteVersionsRange(fromVersion, toVersion); err != nil {
		return err
	}

	if err := tree.ndb.Commit(); err != nil {
		return err
	}

	tree.mtx.Lock()
	defer tree.mtx.Unlock()
	for version := fromVersion; version < toVersion; version++ {
		delete(tree.versions, version)
	}

	return nil
}

// DeleteVersion deletes a tree version from disk. The version can then no
// longer be accessed.
func (tree *MutableTree) DeleteVersion(version int64) error {
	debug("DELETE VERSION: %d\n", version)

	if err := tree.deleteVersion(version); err != nil {
		return err
	}

	if err := tree.ndb.Commit(); err != nil {
		return err
	}

	tree.mtx.Lock()
	defer tree.mtx.Unlock()
	delete(tree.versions, version)
	return nil
}

// Rotate right and return the new node and orphan.
func (tree *MutableTree) rotateRight(node *Node) (*Node, *Node) {
	version := tree.version + 1

	// TODO: optimize balance & rotate.
	node = node.clone(version)
	orphaned := node.getLeftNode(tree.ImmutableTree)
	newNode := orphaned.clone(version)

	newNoderHash, newNoderCached := newNode.rightHash, newNode.rightNode
	newNode.rightHash, newNode.rightNode = node.hash, node
	node.leftHash, node.leftNode = newNoderHash, newNoderCached

	node.calcHeightAndSize(tree.ImmutableTree)
	newNode.calcHeightAndSize(tree.ImmutableTree)

	return newNode, orphaned
}

// Rotate left and return the new node and orphan.
func (tree *MutableTree) rotateLeft(node *Node) (*Node, *Node) {
	version := tree.version + 1

	// TODO: optimize balance & rotate.
	node = node.clone(version)
	orphaned := node.getRightNode(tree.ImmutableTree)
	newNode := orphaned.clone(version)

	newNodelHash, newNodelCached := newNode.leftHash, newNode.leftNode
	newNode.leftHash, newNode.leftNode = node.hash, node
	node.rightHash, node.rightNode = newNodelHash, newNodelCached

	node.calcHeightAndSize(tree.ImmutableTree)
	newNode.calcHeightAndSize(tree.ImmutableTree)

	return newNode, orphaned
}

// NOTE: assumes that node can be modified
// TODO: optimize balance & rotate
func (tree *MutableTree) balance(node *Node, orphans *[]*Node) (newSelf *Node) {
	if node.persisted {
		panic("Unexpected balance() call on persisted node")
	}
	balance := node.calcBalance(tree.ImmutableTree)

	if balance > 1 {
		if node.getLeftNode(tree.ImmutableTree).calcBalance(tree.ImmutableTree) >= 0 {
			// Left Left Case
			newNode, orphaned := tree.rotateRight(node)
			*orphans = append(*orphans, orphaned)
			return newNode
		}
		// Left Right Case
		var leftOrphaned *Node

		left := node.getLeftNode(tree.ImmutableTree)
		node.leftHash = nil
		node.leftNode, leftOrphaned = tree.rotateLeft(left)
		newNode, rightOrphaned := tree.rotateRight(node)
		*orphans = append(*orphans, left, leftOrphaned, rightOrphaned)
		return newNode
	}
	if balance < -1 {
		if node.getRightNode(tree.ImmutableTree).calcBalance(tree.ImmutableTree) <= 0 {
			// Right Right Case
			newNode, orphaned := tree.rotateLeft(node)
			*orphans = append(*orphans, orphaned)
			return newNode
		}
		// Right Left Case
		var rightOrphaned *Node

		right := node.getRightNode(tree.ImmutableTree)
		node.rightHash = nil
		node.rightNode, rightOrphaned = tree.rotateRight(right)
		newNode, leftOrphaned := tree.rotateLeft(node)

		*orphans = append(*orphans, right, leftOrphaned, rightOrphaned)
		return newNode
	}
	// Nothing changed
	return node
}

func (tree *MutableTree) addOrphans(orphans []*Node) {
	for _, node := range orphans {
		if !node.persisted {
			// We don't need to orphan nodes that were never persisted.
			continue
		}
		if len(node.hash) == 0 {
			panic("Expected to find node hash, but was empty")
		}
		tree.orphans[string(node.hash)] = node.version
	}
}
