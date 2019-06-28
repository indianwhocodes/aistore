// Package cluster provides common interfaces and local access to cluster-level metadata
/*
 * Copyright (c) 2018, NVIDIA CORPORATION. All rights reserved.
 */
package cluster

import (
	"fmt"
	"os"
	"strconv"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/NVIDIA/aistore/3rdparty/atomic"
	"github.com/NVIDIA/aistore/3rdparty/glog"
	"github.com/NVIDIA/aistore/cmn"
	"github.com/NVIDIA/aistore/fs"
	"github.com/NVIDIA/aistore/ios"
	"github.com/NVIDIA/aistore/memsys"
)

//
// Local Object Metadata (LOM) is a locally stored object metadata comprising:
// - version, atime, checksum, size, etc. object attributes and flags
// - user and internally visible object names
// - associated runtime context including properties and configuration of the
//   bucket that contains the object, etc.
//

const pkgName = "cluster"

type (
	// NOTE: sizeof(lmeta) = 72 as of 4/16
	lmeta struct {
		uname   string
		version string
		size    int64
		atime   int64
		atimefs int64
		bckID   uint64
		cksum   cmn.Cksummer // ReCache(ref)
		copies  fs.MPI       // ditto
	}
	LOM struct {
		// local meta
		md lmeta
		// other names
		FQN             string
		Bucket, Objname string
		HrwFQN          string // misplaced?
		// runtime context
		T          Target
		config     *cmn.Config
		bmd        *BMD
		BckProps   *cmn.BucketProps
		ParsedFQN  fs.ParsedFQN // redundant in-part; tradeoff to speed-up workfile name gen, etc.
		BckIsLocal bool         // the bucket (that contains this object) is local
		BadCksum   bool         // this object has a bad checksum
		exists     bool         // determines if the object exists or not (initially set by fstat)
		loaded     bool
	}
	LomCacheRunner struct {
		cmn.Named
		mem2    *memsys.Mem2
		T       Target
		stopCh  chan struct{}
		stopped atomic.Bool
	}
)

func init() {
	if logLvl, ok := cmn.CheckDebug(pkgName); ok {
		glog.SetV(glog.SmoduleCluster, logLvl)
	}
}

//
// LOM public methods
//

func (lom *LOM) Uname() string               { return lom.md.uname }
func (lom *LOM) BMD() *BMD                   { return lom.bmd }
func (lom *LOM) Size() int64                 { return lom.md.size }
func (lom *LOM) SetSize(size int64)          { lom.md.size = size }
func (lom *LOM) Version() string             { return lom.md.version }
func (lom *LOM) SetVersion(ver string)       { lom.md.version = ver }
func (lom *LOM) Cksum() cmn.Cksummer         { return lom.md.cksum }
func (lom *LOM) SetCksum(cksum cmn.Cksummer) { lom.md.cksum = cksum }
func (lom *LOM) Atime() time.Time            { return time.Unix(0, lom.md.atime) }
func (lom *LOM) AtimeUnix() int64            { return lom.md.atime }
func (lom *LOM) SetAtimeUnix(tu int64)       { lom.md.atime = tu }
func (lom *LOM) ECEnabled() bool             { return lom.BckProps.EC.Enabled }
func (lom *LOM) LRUEnabled() bool            { return lom.BckProps.LRU.Enabled }
func (lom *LOM) Misplaced() bool             { return lom.HrwFQN != lom.FQN && !lom.IsCopy() } // misplaced (subj to rebalancing)
func (lom *LOM) SetBMD(bmd *BMD)             { lom.bmd = bmd }                                 // NOTE: internal use!
func (lom *LOM) SetBID(bid uint64)           { lom.md.bckID = bid }                            // ditto

func (lom *LOM) Config() *cmn.Config {
	if lom.config == nil {
		lom.config = cmn.GCO.Get()
	}
	return lom.config
}
func (lom *LOM) MirrorConf() *cmn.MirrorConf { return &lom.BckProps.Mirror }
func (lom *LOM) CksumConf() *cmn.CksumConf {
	conf := &lom.BckProps.Cksum
	if conf.Type == cmn.PropInherit {
		conf = &lom.Config().Cksum
	}
	return conf
}
func (lom *LOM) VerConf() *cmn.VersionConf {
	conf := &lom.BckProps.Versioning
	if conf.Type == cmn.PropInherit {
		conf = &lom.Config().Ver
	}
	return conf
}

//
// local copy management
//
func (lom *LOM) HasCopies() bool   { return !lom.IsCopy() && lom.NumCopies() > 1 }
func (lom *LOM) NumCopies() int    { return len(lom.md.copies) + 1 }
func (lom *LOM) GetCopies() fs.MPI { return lom.md.copies }
func (lom *LOM) IsCopy() bool {
	if len(lom.md.copies) != 1 {
		return false
	}
	_, ok := lom.md.copies[lom.HrwFQN]
	return ok
}
func (lom *LOM) SetCopies(cpyfqn string, mpi *fs.MountpathInfo) {
	lom.md.copies = make(fs.MPI, 1)
	lom.md.copies[cpyfqn] = mpi
}
func (lom *LOM) AddCopy(cpyfqn string, mpi *fs.MountpathInfo) {
	if lom.md.copies == nil {
		lom.SetCopies(cpyfqn, mpi)
	} else {
		_, ok := lom.md.copies[cpyfqn]
		cmn.Assert(!ok) // DEBUG  FIXME -- TODO -- FIXME
		lom.md.copies[cpyfqn] = mpi
	}
}
func (lom *LOM) DelCopy(cpyfqn string) (errstr string) {
	l := len(lom.md.copies)
	if _, ok := lom.md.copies[cpyfqn]; !ok {
		return fmt.Sprintf("lom %s(%d): copy %s %s", lom, l, cpyfqn, cmn.DoesNotExist)
	}
	if l == 1 {
		return lom.DelAllCopies()
	}
	if lom._whingeCopy() {
		return
	}
	delete(lom.md.copies, cpyfqn)
	if err := os.Remove(cpyfqn); err != nil && !os.IsNotExist(err) {
		return err.Error()
	}
	return
}
func (lom *LOM) _whingeCopy() (yes bool) {
	if !lom.IsCopy() {
		return
	}
	errstr := fmt.Sprintf("unexpected: %s([fqn=%s] [hrw=%s] %+v)", lom.StringEx(), lom.FQN, lom.HrwFQN, lom.md.copies)
	cmn.DassertMsg(false, errstr, pkgName)
	glog.Errorln(errstr)
	return true
}
func (lom *LOM) DelAllCopies() (errstr string) {
	if lom._whingeCopy() {
		return
	}
	if !lom.HasCopies() {
		return
	}
	n := 0
	for cpyfqn := range lom.md.copies {
		if err := os.Remove(cpyfqn); err != nil && !os.IsNotExist(err) {
			errstr = err.Error()
			continue
		}
		delete(lom.md.copies, cpyfqn)
		n++
	}
	if n < len(lom.md.copies) {
		glog.Errorf("%s: failed to remove some copies(%d < %d), err: %s", lom, n, len(lom.md.copies), errstr)
	} else {
		lom.md.copies = nil
	}
	return
}

func (lom *LOM) CopyObject(dstFQN string, buf []byte) (dst *LOM, err error) {
	if lom.IsCopy() {
		err = fmt.Errorf("%s is a copy", lom)
		return
	}
	dst = lom.Clone(dstFQN)
	err = cmn.CopyFile(lom.FQN, dst.FQN, buf)
	return
}

//
// lom.String() and helpers
//

func (lom *LOM) String() string { return lom._string(lom.Bucket) }

func (lom *LOM) _string(b string) string {
	var (
		a string
		s = fmt.Sprintf("o[%s/%s fs=%s", b, lom.Objname, lom.ParsedFQN.MpathInfo.FileSystem)
	)
	if glog.FastV(4, glog.SmoduleCluster) {
		s += fmt.Sprintf("(%s)", lom.FQN)
		if lom.md.size != 0 {
			s += " size=" + cmn.B2S(lom.md.size, 1)
		}
		if lom.md.version != "" {
			s += " ver=" + lom.md.version
		}
		if lom.md.cksum != nil {
			s += " " + lom.md.cksum.String()
		}
	}
	if !lom.loaded {
		a = "(-)"
	} else if !lom.exists {
		a = "(x)"
	} else {
		if lom.Misplaced() {
			a += "(misplaced)"
		}
		if lom.IsCopy() {
			a += "(copy)"
		}
		if n := lom.NumCopies(); n > 1 {
			a += fmt.Sprintf("(%dc)", n)
		}
		if lom.BadCksum {
			a += "(bad-cksum)"
		}
	}
	return s + a + "]"
}

func (lom *LOM) bckString() string { return lom.bmd.Bstring(lom.Bucket, lom.BckIsLocal) }

func (lom *LOM) StringEx() string {
	if lom.bmd == nil {
		return lom.String()
	}
	return lom._string(lom.bckString())
}

func (lom *LOM) BadCksumErr(cksum cmn.Cksummer) (errstr string) {
	return cmn.BadCksum(cksum, lom.md.cksum) + ", " + lom.StringEx()
}

// IncObjectVersion increments the current version xattrs and returns the new value.
// If the current version is empty (local bucket versioning (re)enabled, new file)
// the version is set to "1"
func (lom *LOM) IncObjectVersion() (newVersion string, errstr string) {
	const initialVersion = "1"
	if !lom.exists {
		newVersion = initialVersion
		return
	}
	md, err := lom.lmfs(false)
	if err != nil {
		return "", err.Error()
	}
	if md.version == "" {
		return initialVersion, ""
	}
	numVersion, err := strconv.Atoi(md.version)
	if err != nil {
		return "", err.Error()
	}
	return fmt.Sprintf("%d", numVersion+1), ""
}

// best-effort GET load balancing (see also mirror.findLeastUtilized())
func (lom *LOM) LoadBalanceGET(now time.Time) (fqn string) {
	if len(lom.md.copies) == 0 {
		return lom.FQN
	}
	return fs.Mountpaths.LoadBalanceGET(lom.FQN, lom.ParsedFQN.MpathInfo.Path, lom.md.copies, now)
}

// Returns stored checksum (if present) and computed checksum (if requested)
// MAY compute and store a missing (xxhash) checksum.
// If xattr checksum is different than lom's metadata checksum, returns error
// and do not recompute checksum even if recompute set to true.
//
// * objects are stored in the cluster with their content checksums and in accordance
//   with their bucket configurations.
// * xxhash is the system-default checksum.
// * user can override the system default on a bucket level, by setting checksum=none.
// * bucket (re)configuration can be done at any time.
// * an object with a bad checksum cannot be retrieved (via GET) and cannot be replicated
//   or migrated.
// * GET and PUT operations support an option to validate checksums.
// * validation is done against a checksum stored with an object (GET), or a checksum
//   provided by a user (PUT).
// * replications and migrations are always protected by checksums.
// * when two objects in the cluster have identical (bucket, object) names and checksums,
//   they are considered to be full replicas of each other.
// ==============================================================================
func (lom *LOM) ValidateChecksum(recompute bool) (errstr string) {
	var (
		storedCksum string
		cksumType   = lom.CksumConf().Type
	)
	if cksumType == cmn.ChecksumNone {
		return
	}
	if lom.md.cksum != nil {
		_, storedCksum = lom.md.cksum.Get()
		cmn.Assert(storedCksum != "")
	}
	{ // FIXME: single xattr-meta
		var v cmn.Cksummer
		md, err := lom.lmfs(false)
		if err != nil {
			return err.Error()
		}
		if md != nil {
			v = md.cksum
		}
		if !recompute && lom.md.cksum == nil && v == nil {
			return
		}
		// both checksums were missing and recompute requested, go immediately to computing
		recomputeEmptyCksms := recompute && v == nil && lom.md.cksum == nil

		if !recomputeEmptyCksms && !cmn.EqCksum(lom.md.cksum, v) {
			lom.BadCksum = true
			errstr = lom.BadCksumErr(v)
			lom.Uncache()
			return
		}
	}
	if storedCksum != "" && !recompute {
		return
	}

	return lom.ValidateDiskChecksum()
}

// ValidateDiskChecksum validates if checksum stored in lom's metadata matches checksum
// of object's content stored on a disk. It does not check if lom's metadata checksum matches with
// checksum in lom's xattributes. This function is supposed to be called only when we know that
// xattr and meta checksums match, and we want to save one FS read
func (lom *LOM) ValidateDiskChecksum() (errstr string) {
	var (
		storedCksum, computedCksum string
		cksumType                  = lom.CksumConf().Type
	)

	if cksumType == cmn.ChecksumNone {
		return
	}

	if lom.md.cksum != nil {
		_, storedCksum = lom.md.cksum.Get()
		cmn.Assert(storedCksum != "")
	}

	// compute
	cmn.Assert(cksumType == cmn.ChecksumXXHash) // sha256 et al. not implemented yet
	if computedCksum, errstr = lom.computeXXHash(lom.FQN, lom.md.size); errstr != "" {
		return
	}
	if storedCksum == "" {
		oldCksm := lom.md.cksum
		lom.md.cksum = cmn.NewCksum(cksumType, computedCksum)
		if err := lom.Persist(); err != nil {
			lom.md.cksum = oldCksm
			return err.Error()
		}

		lom.ReCache()
		return
	}
	v := cmn.NewCksum(cksumType, computedCksum)
	if !cmn.EqCksum(lom.md.cksum, v) {
		lom.BadCksum = true
		errstr = lom.BadCksumErr(v)
		lom.Uncache()
	}

	return
}

func (lom *LOM) CksumComputeIfMissing() (cksum cmn.Cksummer, errstr string) {
	var (
		val       string
		cksumType = lom.CksumConf().Type
	)
	if cksumType == cmn.ChecksumNone {
		return
	}
	if lom.md.cksum != nil {
		cksum = lom.md.cksum
		return
	}
	val, errstr = lom.computeXXHash(lom.FQN, lom.md.size)
	if errstr != "" {
		return
	}
	cksum = cmn.NewCksum(cmn.ChecksumXXHash, val)
	return
}

func (lom *LOM) computeXXHash(fqn string, size int64) (cksumstr, errstr string) {
	file, err := os.Open(fqn)
	if err != nil {
		errstr = fmt.Sprintf("%s, err: %v", fqn, err)
		return
	}
	buf, slab := lom.T.GetMem2().AllocForSize(size)
	cksumstr, errstr = cmn.ComputeXXHash(file, buf)
	file.Close()
	slab.Free(buf)
	return
}

//
// private methods
//
func (lom *LOM) Clone(fqn string) *LOM {
	dst := &LOM{}
	*dst = *lom
	dst.FQN = fqn
	return dst
}

func (lom *LOM) init(bckProvider string) (errstr string) {
	var bckPresent, fromFQN bool
	bowner := lom.T.GetBowner()
	if bckProvider != "" {
		val, err := cmn.ProviderFromStr(bckProvider)
		if err != nil {
			return err.Error()
		}
		lom.BckIsLocal = cmn.IsProviderLocal(val)
		bckProvider = val
	}
	if lom.Bucket == "" || lom.Objname == "" {
		if bckProvider != "" {
			lom.ParsedFQN, lom.HrwFQN, errstr = ResolveFQN(lom.FQN, nil, lom.BckIsLocal)
		} else {
			lom.ParsedFQN, lom.HrwFQN, errstr = ResolveFQN(lom.FQN, bowner)
			bckProvider = cmn.ProviderFromLoc(lom.ParsedFQN.IsLocal)
		}
		if errstr != "" {
			return
		}
		lom.Bucket, lom.Objname = lom.ParsedFQN.Bucket, lom.ParsedFQN.Objname
		lom.BckIsLocal = lom.ParsedFQN.IsLocal
		fromFQN = true
	}
	lom.md.uname = Bo2Uname(lom.Bucket, lom.Objname)
	// bucketmd, local, bprops
	lom.bmd = bowner.Get()
	if bckProvider == "" {
		lom.BckIsLocal = lom.bmd.IsLocal(lom.Bucket)
	}
	lom.BckProps, bckPresent = lom.bmd.Get(lom.Bucket, lom.BckIsLocal)
	if lom.BckIsLocal && !bckPresent {
		return fmt.Sprintf("%s local bucket %s", lom.Bucket, cmn.DoesNotExist)
	}
	if !fromFQN {
		lom.ParsedFQN.MpathInfo, lom.ParsedFQN.Digest, errstr = hrwMpath(lom.Bucket, lom.Objname)
		if errstr != "" {
			return
		}
		lom.ParsedFQN.ContentType = fs.ObjectType
		lom.FQN = fs.CSM.FQN(lom.ParsedFQN.MpathInfo, fs.ObjectType, lom.BckIsLocal, lom.Bucket, lom.Objname)
		lom.HrwFQN = lom.FQN
		lom.ParsedFQN.IsLocal = lom.BckIsLocal
		lom.ParsedFQN.Bucket, lom.ParsedFQN.Objname = lom.Bucket, lom.Objname
	}
	return
}

// Local Object Metadata (LOM) - is cached. Respectively, lifecycle of any given LOM
// instance includes the following steps:
//
// 1) construct LOM instance and initialize its runtime state: lom = LOM{...}.Init()
// 2) load persistent state (aka lmeta) from one of the LOM caches or the underlying
//    filesystem: lom.Load(true/false)
//    Load(false) also entails removing this LOM from cache, if exists -
//    useful when you are going delete the corresponding data object, for instance
//
// 3) use: via lom.Atime(), lom.Cksum(), lom.Exists() and numerous other accessors
//    It is illegal to check LOM's existence and, generally, do almost anything
//    with it prior to loading - see previous
// 4) update persistent state in memory: lom.Set*() methods
//    (requires subsequent re-caching via lom.ReCache())
// 5) update persistent state on disk: lom.Persist()
// 6) remove a given LOM instance from cache: lom.Uncache()
// 7) evict an entire bucket-load of LOM cache: cluster.EvictCache(bucket)
//
// 8) periodic (lazy) eviction followed by access-time synchronization: see LomCacheRunner
// =======================================================================================
func (lom *LOM) Hkey() (string, int) {
	cmn.Dassert(lom.ParsedFQN.Digest != 0, pkgName)
	return lom.md.uname, int(lom.ParsedFQN.Digest & fs.LomCacheMask)
}
func (newlom LOM) Init(bckProvider string, config ...*cmn.Config) (lom *LOM, errstr string) {
	lom = &newlom
	if errstr = lom.init(bckProvider); errstr != "" {
		return
	}
	if len(config) > 0 {
		lom.config = config[0]
	}
	return
}

func (lom *LOM) IsLoaded() (ok bool) {
	var (
		hkey, idx = lom.Hkey()
		cache     = lom.ParsedFQN.MpathInfo.LomCache(idx)
	)
	_, ok = cache.M.Load(hkey)
	return
}

func (lom *LOM) Load(add bool) (fromCache bool, errstr string) {
	// fast path
	var (
		hkey, idx = lom.Hkey()
		cache     = lom.ParsedFQN.MpathInfo.LomCache(idx)
	)
	lom.loaded = true
	if lom.FQN == lom.HrwFQN {
		if md, ok := cache.M.Load(hkey); ok {
			fromCache = true
			lom.exists = true
			lmeta := md.(*lmeta)
			lom.md = *lmeta
			if !add { // uncache
				cache.M.Delete(hkey)
			}
			if lom.existsInBucket() {
				return
			}
		}
	} else {
		add = false
	}
	// slow path
	errstr = lom.FromFS()
	if errstr == "" && lom.exists {
		md := &lmeta{}
		lom.md.bckID = lom.BckProps.BID
		*md = lom.md
		if !add { // ditto
			return
		}
		cache.M.Store(hkey, md)
	}
	return
}

func (lom *LOM) Exists() bool {
	cmn.Assert(lom.loaded)
	return lom.existsInBucket()
}
func (lom *LOM) existsInBucket() bool {
	if lom.BckIsLocal && lom.exists && !lom.bmd.Exists(lom.Bucket, lom.md.bckID, lom.BckIsLocal) {
		lom.Uncache()
		lom.exists = false
		return false
	}
	return lom.exists
}

func (lom *LOM) ReCache() {
	cmn.Assert(lom.FQN == lom.HrwFQN) // DEBUG not caching copies
	var (
		hkey, idx = lom.Hkey()
		cache     = lom.ParsedFQN.MpathInfo.LomCache(idx)
		md        = &lmeta{}
	)
	*md = lom.md
	md.bckID = lom.BckProps.BID
	cache.M.Store(hkey, md)
	lom.loaded = true
}

func (lom *LOM) Uncache() {
	cmn.Assert(lom.FQN == lom.HrwFQN) // DEBUG not caching copies
	var (
		hkey, idx = lom.Hkey()
		cache     = lom.ParsedFQN.MpathInfo.LomCache(idx)
	)
	cache.M.Delete(hkey)
}

func (lom *LOM) FromFS() (errstr string) {
	lom.exists = true
	if _, err := lom.lmfs(true); err != nil {
		lom.exists = false
		if err == syscall.ENODATA || err == syscall.ENOENT {
			if _, errex := os.Stat(lom.FQN); os.IsNotExist(errex) {
				return
			}
		}
		if !os.IsNotExist(err) {
			errstr = fmt.Sprintf("%s: errmeta %v", lom.StringEx(), err)
			lom.T.FSHC(err, lom.FQN)
		}
		return
	}
	// fstat & atime
	finfo, err := os.Stat(lom.FQN)
	if err != nil {
		lom.exists = false
		if !os.IsNotExist(err) {
			errstr = fmt.Sprintf("%s: errstat %v", lom.StringEx(), err)
			lom.T.FSHC(err, lom.FQN)
		}
		return
	}
	if lom.md.size != finfo.Size() { // corruption or tampering
		errstr = fmt.Sprintf("%s: errsize (%d != %d)", lom.StringEx(), lom.md.size, finfo.Size())
		return
	}
	atime := ios.GetATime(finfo)
	lom.md.atime = atime.UnixNano()
	lom.md.atimefs = lom.md.atime
	return
}

//
// evict lom cache
//
func EvictCache(bucket string) {
	var (
		caches = lomCaches()
		wg     = &sync.WaitGroup{}
	)
	for _, cache := range caches {
		wg.Add(1)
		go func(cache *fs.LomCache) {
			fevict := func(hkey, _ interface{}) bool {
				uname := hkey.(string)
				b, _ := Uname2Bo(uname)
				if bucket == b {
					cache.M.Delete(hkey)
				}
				return true
			}
			cache.M.Range(fevict)
			wg.Done()
		}(cache)
	}
	wg.Wait()
}

//
// lom cache runner
//
const (
	oomEvictAtime = time.Minute      // OOM
	oomTimeIntval = time.Second * 10 // ===/===
	mpeEvictAtime = time.Minute * 5  // extreme
	mpeTimeIntval = time.Minute      // ===/===
	mphEvictAtime = time.Minute * 10 // high
	mphTimeIntval = time.Minute * 2  // ===/===
	mpnEvictAtime = time.Hour        // normal
	mpnTimeIntval = time.Minute * 10 // ===/===
	minSize2Evict = cmn.KiB * 256
)

func NewLomCacheRunner(mem2 *memsys.Mem2, t Target) *LomCacheRunner {
	return &LomCacheRunner{
		T:      t,
		mem2:   mem2,
		stopCh: make(chan struct{}, 1),
	}
}

func (r *LomCacheRunner) Run() error {
	var (
		d     time.Duration
		timer = time.NewTimer(time.Minute * 10)
		md    = lmeta{}
		minev = int(minSize2Evict / unsafe.Sizeof(md))
	)
	for {
		select {
		case <-timer.C:
			switch p := r.mem2.MemPressure(); p { // TODO: heap-memory-arbiter (HMA) abstraction TBD
			case memsys.OOM:
				d = oomEvictAtime
			case memsys.MemPressureExtreme:
				d = mpeEvictAtime
			case memsys.MemPressureHigh:
				d = mphEvictAtime
			default:
				d = mpnEvictAtime
			}
			evicted, total := r.work(d)
			if evicted < minev {
				d = mpnTimeIntval
			} else {
				switch p := r.mem2.MemPressure(); p {
				case memsys.OOM:
					d = oomTimeIntval
				case memsys.MemPressureExtreme:
					d = mpeTimeIntval
				case memsys.MemPressureHigh:
					d = mphTimeIntval
				default:
					d = mpnTimeIntval
				}
				timer.Reset(d)
			}
			cmn.Assert(total >= evicted)
			glog.Infof("total %d, evicted %d, timer %v", total-evicted, evicted, d)
		case <-r.stopCh:
			r.stopped.Store(true)
			timer.Stop()
			return nil
		}
	}
}
func (r *LomCacheRunner) Stop(err error) {
	r.stopCh <- struct{}{}
	glog.Infof("Stopping %s, err: %v", r.Getname(), err)
}
func (r *LomCacheRunner) isStopping() bool { return r.stopped.Load() }

func (r *LomCacheRunner) work(d time.Duration) (numevicted, numtotal int) {
	var (
		caches         = lomCaches()
		now            = time.Now()
		wg             = &sync.WaitGroup{}
		bmd            = r.T.GetBowner().Get()
		evicted, total atomic.Uint32
	)
	for _, cache := range caches {
		wg.Add(1)
		go func(cache *fs.LomCache) {
			feviat := func(hkey, value interface{}) bool {
				if r.isStopping() {
					return false
				}
				var (
					md    = value.(*lmeta)
					atime = time.Unix(0, md.atime)
				)
				total.Add(1)
				if now.Sub(atime) < d {
					return true
				}
				if md.atime != md.atimefs {
					if lom, errstr := lomFromLmeta(md, bmd); errstr == "" {
						lom.flushAtime(atime)
					}
					// TODO: throttle via mountpath.IsIdle(), etc.
				}
				cache.M.Delete(hkey)
				evicted.Add(1)
				return true
			}
			cache.M.Range(feviat)
			wg.Done()
		}(cache)
	}
	wg.Wait()
	numevicted, numtotal = int(evicted.Load()), int(total.Load())
	return
}

func (lom *LOM) flushAtime(atime time.Time) {
	finfo, err := os.Stat(lom.FQN)
	if err != nil {
		return
	}
	mtime := finfo.ModTime()
	if err = os.Chtimes(lom.FQN, atime, mtime); err != nil {
		glog.Errorf("%s: flush atime err: %v", lom, err)
	}
}

//
// static helpers
//
func lomFromLmeta(md *lmeta, bmd *BMD) (lom *LOM, errstr string) {
	var (
		bucket, objname = Uname2Bo(md.uname)
		local, exists   bool
	)
	lom = &LOM{Bucket: bucket, Objname: objname}
	if bmd.Exists(bucket, md.bckID, true) {
		local, exists = true, true
	} else if bmd.Exists(bucket, md.bckID, false) {
		local, exists = false, true
	}
	lom.exists = exists
	if exists {
		lom.BckIsLocal = local
		lom.FQN, _, errstr = HrwFQN(fs.ObjectType, lom.Bucket, lom.Objname, lom.BckIsLocal)
	}
	return
}

func lomCaches() []*fs.LomCache {
	availablePaths, _ := fs.Mountpaths.Get()
	var (
		l      = len(availablePaths) * (fs.LomCacheMask + 1)
		caches = make([]*fs.LomCache, l)
	)
	i := 0
	for _, mpathInfo := range availablePaths {
		for idx := 0; idx <= fs.LomCacheMask; idx++ {
			cache := mpathInfo.LomCache(idx)
			caches[i] = cache
			i++
		}
	}
	return caches
}

//
// access perms
//

func (lom *LOM) AllowGET() error  { return lom.bmd.AllowGET(lom.Bucket, lom.BckIsLocal, lom.BckProps) }
func (lom *LOM) AllowHEAD() error { return lom.bmd.AllowHEAD(lom.Bucket, lom.BckIsLocal, lom.BckProps) }
func (lom *LOM) AllowPUT() error  { return lom.bmd.AllowPUT(lom.Bucket, lom.BckIsLocal, lom.BckProps) }
func (lom *LOM) AllowColdGET() error {
	return lom.bmd.AllowColdGET(lom.Bucket, lom.BckIsLocal, lom.BckProps)
}
func (lom *LOM) AllowDELETE() error {
	return lom.bmd.AllowDELETE(lom.Bucket, lom.BckIsLocal, lom.BckProps)
}
