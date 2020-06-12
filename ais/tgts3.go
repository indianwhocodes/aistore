// Package ais provides core functionality for the AIStore object storage.
/*
 * Copyright (c) 2020, NVIDIA CORPORATION. All rights reserved.
 */
package ais

import (
	"context"
	"fmt"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/NVIDIA/aistore/3rdparty/glog"
	"github.com/NVIDIA/aistore/ais/s3compat"
	"github.com/NVIDIA/aistore/cluster"
	"github.com/NVIDIA/aistore/cmn"
	"github.com/NVIDIA/aistore/cmn/debug"
	"github.com/NVIDIA/aistore/ec"
	"github.com/NVIDIA/aistore/fs"
	"github.com/NVIDIA/aistore/tar2tf"
)

// PUT s3/bckName/objName
func (t *targetrunner) s3Handler(w http.ResponseWriter, r *http.Request) {
	apitems, err := t.checkRESTItems(w, r, 0, true, cmn.S3)
	if err != nil {
		return
	}

	switch r.Method {
	case http.MethodHead:
		t.headObjS3(w, r, apitems)
	case http.MethodGet:
		t.getObjS3(w, r, apitems)
	case http.MethodPut:
		t.putObjS3(w, r, apitems)
	case http.MethodDelete:
		t.delObjS3(w, r, apitems)
	default:
		s := fmt.Sprintf("Invalid HTTP Method: %v %s", r.Method, r.URL.Path)
		t.invalmsghdlr(w, r, s)
	}
}

func (t *targetrunner) copyObjS3(w http.ResponseWriter, r *http.Request, items []string) {
	config := cmn.GCO.Get()
	src := r.Header.Get(s3compat.HeaderObjSrc)
	src = strings.Trim(src, "/") // in AWS examples the path starts with "/"
	parts := strings.SplitN(src, "/", 2)
	if len(parts) < 2 {
		t.invalmsghdlr(w, r, "copy is not an object name")
		return
	}
	bckSrc := cluster.NewBck(parts[0], cmn.ProviderAIS, cmn.NsGlobal)
	objSrc := strings.Trim(parts[1], "/")
	if err := bckSrc.Init(t.owner.bmd, nil); err != nil {
		t.invalmsghdlr(w, r, err.Error())
		return
	}
	lom := &cluster.LOM{T: t, ObjName: objSrc}
	if err := lom.Init(bckSrc.Bck, config); err != nil {
		if _, ok := err.(*cmn.ErrorRemoteBucketDoesNotExist); ok {
			t.BMDVersionFixup(r, cmn.Bck{}, true /* sleep */)
			err = lom.Init(bckSrc.Bck, config)
		}
		if err != nil {
			t.invalmsghdlr(w, r, err.Error())
		}
		return
	}
	if err := lom.Load(); err != nil {
		t.invalmsghdlr(w, r, err.Error())
		return
	}

	bckDst := cluster.NewBck(items[0], cmn.ProviderAIS, cmn.NsGlobal)
	if err := bckDst.Init(t.owner.bmd, nil); err != nil {
		t.invalmsghdlr(w, r, err.Error())
		return
	}
	if len(items) < 2 {
		t.invalmsghdlr(w, r, "object name is undefined")
		return
	}
	var (
		si   *cluster.Snode
		smap = t.owner.smap.get()
		err  error
	)
	objName := path.Join(items[1:]...)
	si, err = cluster.HrwTarget(bckDst.MakeUname(objName), &smap.Smap)
	if err != nil {
		t.invalmsghdlr(w, r, err.Error())
		return
	}

	if si.ID() == t.Snode().ID() {
		err = t.localObjCopy(lom, bckDst.Bck, objName)
	} else {
		lom.Lock(false)
		defer lom.Unlock(false)
		err = t.sendObj(lom, si, bckDst.Bck, objName)
	}
	if err != nil {
		t.invalmsghdlr(w, r, err.Error())
		return
	}
	var cksumValue string
	if cksum := lom.Cksum(); cksum != nil && cksum.Type() == cmn.ChecksumMD5 {
		cksumValue = cksum.Value()
	}
	result := s3compat.CopyObjectResult{
		LastModified: s3compat.FormatTime(lom.Atime()),
		ETag:         cksumValue,
	}
	w.Write(result.MustMarshal())
}

func (t *targetrunner) sendObj(src *cluster.LOM, si *cluster.Snode, bck cmn.Bck, objName string) error {
	if glog.FastV(4, glog.SmoduleAIS) {
		glog.Infof("AISS3 COPY OBJECT: %s/%s => %s [%s/%s]", src.Bck().Bck, src.ObjName, si, bck, objName)
	}
	fh, err := cmn.NewFileHandle(src.FQN)
	if err != nil {
		t.FSHC(err, src.FQN)
		return err
	}
	defer func() {
		debug.AssertNoErr(fh.Close())
	}()

	var (
		query  = cmn.AddBckToQuery(nil, bck)
		header = src.PopulateHdr(nil)
	)
	query.Set(cmn.URLParamProxyID, t.GetSowner().Get().ProxySI.ID())
	args := cmn.ReqArgs{
		Method: http.MethodPut,
		Base:   si.URL(cmn.NetworkIntraData),
		Path:   cmn.URLPath(cmn.Version, cmn.Objects, bck.Name, objName),
		Query:  query,
		Header: header,
		BodyR:  fh,
	}
	req, err := args.Req()
	if err != nil {
		return cmn.NewFailedToCreateHTTPRequest(err)
	}
	req.ContentLength = src.Size()
	resp, err := t.httpclientGetPut.Do(req)
	if err != nil {
		return err
	}
	debug.AssertNoErr(resp.Body.Close())
	return nil
}

func (t *targetrunner) localObjCopy(src *cluster.LOM, bck cmn.Bck, objName string) error {
	if glog.FastV(4, glog.SmoduleAIS) {
		glog.Infof("AISS3 COPY OBJECT(local): %s/%s => %s/%s", src.Bck().Bck, src.ObjName, bck, objName)
	}
	started := time.Now()
	config := cmn.GCO.Get()
	if capInfo := t.AvgCapUsed(config); capInfo.OOS {
		return capInfo.Err
	}
	dstLom := &cluster.LOM{T: t, ObjName: objName}
	if err := dstLom.Init(bck, config); err != nil {
		return err
	}

	dstLom.SetAtimeUnix(started.UnixNano())
	fh, err := cmn.NewFileHandle(src.FQN)
	if err != nil {
		t.FSHC(err, src.FQN)
		return err
	}

	dstLom.SetVersion(src.Version())
	poi := &putObjInfo{
		started: started,
		t:       t,
		lom:     dstLom,
		r:       fh,
		workFQN: fs.CSM.GenContentParsedFQN(dstLom.ParsedFQN, fs.WorkfileType, fs.WorkfilePut),
	}
	err, _ = poi.putObject()
	return err
}

func (t *targetrunner) directPutObjS3(w http.ResponseWriter, r *http.Request, items []string) {
	started := time.Now()
	config := cmn.GCO.Get()
	if capInfo := t.AvgCapUsed(config); capInfo.OOS {
		t.invalmsghdlr(w, r, capInfo.Err.Error())
		return
	}
	bck := cluster.NewBck(items[0], cmn.ProviderAIS, cmn.NsGlobal)
	if err := bck.Init(t.owner.bmd, nil); err != nil {
		t.invalmsghdlr(w, r, err.Error())
		return
	}
	if len(items) < 2 {
		t.invalmsghdlr(w, r, "object name is undefined")
		return
	}
	var (
		err error
	)
	objName := path.Join(items[1:]...)
	lom := &cluster.LOM{T: t, ObjName: objName}
	if err = lom.Init(bck.Bck, config); err != nil {
		if _, ok := err.(*cmn.ErrorRemoteBucketDoesNotExist); ok {
			t.BMDVersionFixup(r, cmn.Bck{}, true /*sleep*/)
			err = lom.Init(bck.Bck, config)
		}
		if err != nil {
			t.invalmsghdlr(w, r, err.Error())
			return
		}
	}
	if lom.Bck().IsAIS() && lom.VerConf().Enabled {
		lom.Load() // need to know the current version if versioning enabled
	}
	lom.SetAtimeUnix(started.UnixNano())

	// TODO: lom.SetCustomMD(cluster.AmazonMD5ObjMD, checksum)

	if err, errCode := t.doPut(r, lom, started); err != nil {
		t.fshc(err, lom.FQN)
		t.invalmsghdlr(w, r, err.Error(), errCode)
		return
	}
}

// PUT s3/bckName/objName
func (t *targetrunner) putObjS3(w http.ResponseWriter, r *http.Request, items []string) {
	if r.Header.Get(s3compat.HeaderObjSrc) == "" {
		t.directPutObjS3(w, r, items)
		return
	}
	t.copyObjS3(w, r, items)
}

// GET s3/bckName/objName[!tf]
func (t *targetrunner) getObjS3(w http.ResponseWriter, r *http.Request, items []string) {
	if len(items) < 2 {
		t.invalmsghdlr(w, r, "object name is undefined")
		return
	}
	started := time.Now()
	config := cmn.GCO.Get()
	bck := cluster.NewBck(items[0], cmn.ProviderAIS, cmn.NsGlobal)
	if err := bck.Init(t.owner.bmd, nil); err != nil {
		t.invalmsghdlr(w, r, err.Error())
		return
	}
	var (
		err          error
		objSize      int64
		objName, tag string
	)
	// TODO: remove
	if objName, tag = cmn.S3ObjNameTag(path.Join(items[1:]...)); tag != "" {
		if tag != cmn.TF {
			t.invalmsghdlr(w, r, fmt.Sprintf("invalid tag=%q (expecting %q)", tag, cmn.TF))
			return
		}
		if !cmn.HasTarExtension(objName) {
			a := []string{cmn.ExtTar, cmn.ExtTarTgz, cmn.ExtTarTgz}
			t.invalmsghdlr(w, r, fmt.Sprintf("invalid name %s: expecting one of %v extensions", objName, a))
			return
		}
	}

	lom := &cluster.LOM{T: t, ObjName: objName}
	if err = lom.Init(bck.Bck, config); err != nil {
		if _, ok := err.(*cmn.ErrorRemoteBucketDoesNotExist); ok {
			t.BMDVersionFixup(r, cmn.Bck{}, true /* sleep */)
			err = lom.Init(bck.Bck, config)
		}
		if err != nil {
			t.invalmsghdlr(w, r, err.Error())
		}
		return
	}
	if err = lom.Load(true); err != nil {
		t.invalmsghdlr(w, r, err.Error())
		return
	}

	objSize = lom.Size()
	if tag != "" {
		objSize, err = tar2tf.Cache.GetSize(lom)
		if err != nil {
			t.invalmsghdlr(w, r, err.Error())
			return
		}
	}
	goi := &getObjInfo{
		started: started,
		t:       t,
		lom:     lom,
		w:       w,
		ctx:     context.Background(),
		ranges:  cmn.RangesQuery{Range: r.Header.Get(cmn.HeaderRange), Size: objSize},
		tag:     tag,
	}
	s3compat.SetHeaderFromLOM(w.Header(), lom, objSize)
	if err, errCode := goi.getObject(); err != nil {
		if cmn.IsErrConnectionReset(err) {
			glog.Errorf("GET %s: %v", lom, err)
		} else {
			t.invalmsghdlr(w, r, err.Error(), errCode)
		}
	}
}

// HEAD s3/bckName/objName
func (t *targetrunner) headObjS3(w http.ResponseWriter, r *http.Request, items []string) {
	var (
		err    error
		config = cmn.GCO.Get()
	)
	if len(items) < 2 {
		t.invalmsghdlr(w, r, "object name is undefined")
		return
	}
	bucket, objName := items[0], path.Join(items[1:]...)
	bck := cluster.NewBck(bucket, cmn.ProviderAIS, cmn.NsGlobal)
	if err := bck.Init(t.owner.bmd, nil); err != nil {
		t.invalmsghdlr(w, r, err.Error())
		return
	}
	lom := &cluster.LOM{T: t, ObjName: objName}
	if err = lom.Init(bck.Bck, config); err != nil {
		if _, ok := err.(*cmn.ErrorRemoteBucketDoesNotExist); ok {
			t.BMDVersionFixup(r, cmn.Bck{}, true /* sleep */)
			err = lom.Init(bck.Bck, config)
		}
		if err != nil {
			t.invalmsghdlr(w, r, err.Error())
		}
		return
	}

	lom.Lock(false)
	if err = lom.Load(true); err != nil && !cmn.IsObjNotExist(err) { // (doesnotexist -> ok, other)
		lom.Unlock(false)
		t.invalmsghdlr(w, r, err.Error())
		return
	}
	lom.Unlock(false)

	exists := err == nil
	if !exists {
		t.invalmsghdlr(w, r, fmt.Sprintf("%s/%s %s", bucket, objName, cmn.DoesNotExist), http.StatusNotFound)
		return
	}
	s3compat.SetHeaderFromLOM(w.Header(), lom, lom.Size())
}

// DEL s3/bckName/objName
func (t *targetrunner) delObjS3(w http.ResponseWriter, r *http.Request, items []string) {
	var (
		config = cmn.GCO.Get()
		bck    = cluster.NewBck(items[0], cmn.ProviderAIS, cmn.NsGlobal)
	)
	if err := bck.Init(t.owner.bmd, nil); err != nil {
		t.invalmsghdlr(w, r, err.Error())
		return
	}
	if len(items) < 2 {
		t.invalmsghdlr(w, r, "object name is undefined")
		return
	}
	objName := path.Join(items[1:]...)
	lom := &cluster.LOM{T: t, ObjName: objName}
	if err := lom.Init(bck.Bck, config); err != nil {
		t.invalmsghdlr(w, r, err.Error())
		return
	}
	err, errCode := t.objDelete(context.Background(), lom, false)
	if err != nil {
		if errCode == http.StatusNotFound {
			t.invalmsghdlrsilent(w, r,
				fmt.Sprintf("object %s/%s doesn't exist", lom.Bck(), lom.ObjName),
				http.StatusNotFound,
			)
		} else {
			t.invalmsghdlr(w, r, fmt.Sprintf("error deleting %s: %v", lom, err), errCode)
		}
		return
	}
	// EC cleanup if EC is enabled
	ec.ECM.CleanupObject(lom)
}
