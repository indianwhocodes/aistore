// Package api provides AIStore API over HTTP(S)
/*
 * Copyright (c) 2018-2021, NVIDIA CORPORATION. All rights reserved.
 */
package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"github.com/NVIDIA/aistore/cmn"
	"github.com/NVIDIA/aistore/cmn/cos"
	"github.com/NVIDIA/aistore/etl"
	jsoniter "github.com/json-iterator/go"
)

func ETLInit(baseParams BaseParams, msg etl.InitMsg) (id string, err error) {
	baseParams.Method = http.MethodPut
	reqParams := allocRp()
	{
		reqParams.BaseParams = baseParams
		reqParams.Path = cmn.URLPathETL.S
		reqParams.Body = cos.MustMarshal(msg)
	}
	err = reqParams.DoHTTPReqResp(&id)
	freeRp(reqParams)
	return id, err
}

func ETLList(baseParams BaseParams) (list []etl.Info, err error) {
	baseParams.Method = http.MethodGet
	reqParams := allocRp()
	{
		reqParams.BaseParams = baseParams
		reqParams.Path = cmn.URLPathETL.S
	}
	err = reqParams.DoHTTPReqResp(&list)
	freeRp(reqParams)
	return list, err
}

func ETLLogs(baseParams BaseParams, id string, targetID ...string) (logs etl.PodsLogsMsg, err error) {
	baseParams.Method = http.MethodGet
	var path string
	if len(targetID) > 0 && targetID[0] != "" {
		path = cmn.URLPathETL.Join(id, cmn.ETLLogs, targetID[0])
	} else {
		path = cmn.URLPathETL.Join(id, cmn.ETLLogs)
	}
	reqParams := allocRp()
	{
		reqParams.BaseParams = baseParams
		reqParams.Path = path
	}
	err = reqParams.DoHTTPReqResp(&logs)
	freeRp(reqParams)
	return logs, err
}

func ETLHealth(params BaseParams, id string) (healths etl.PodsHealthMsg, err error) {
	params.Method = http.MethodGet
	path := cmn.URLPathETL.Join(id, cmn.ETLHealth)
	reqParams := allocRp()
	{
		reqParams.BaseParams = params
		reqParams.Path = path
	}
	err = reqParams.DoHTTPReqResp(&healths)
	freeRp(reqParams)
	return healths, err
}

func ETLDelete(baseParams BaseParams, id string) (err error) {
	baseParams.Method = http.MethodDelete
	path := cmn.URLPathETL.Join(id)
	reqParams := allocRp()
	{
		reqParams.BaseParams = baseParams
		reqParams.Path = path
	}
	err = reqParams.DoHTTPRequest()
	freeRp(reqParams)
	return
}

func ETLGetInitMsg(params BaseParams, id string) (initMsg etl.InitMsg, err error) {
	params.Method = http.MethodGet
	path := cmn.URLPathETL.Join(id)
	reqParams := allocRp()
	{
		reqParams.BaseParams = params
		reqParams.Path = path
	}
	r, err := reqParams.doReader()
	freeRp(reqParams)
	if err != nil {
		return nil, err
	}
	defer cos.Close(r)

	b, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("failed to read response, err: %w", err)
	}

	var msgInf map[string]json.RawMessage
	if err = jsoniter.Unmarshal(b, &msgInf); err != nil {
		return
	}
	if _, ok := msgInf["code"]; ok {
		initMsg = &etl.InitCodeMsg{}
		err = jsoniter.Unmarshal(b, initMsg)
		return
	}

	if _, ok := msgInf["spec"]; !ok {
		err = fmt.Errorf("invalid response body: %s", b)
		return
	}
	initMsg = &etl.InitSpecMsg{}
	err = jsoniter.Unmarshal(b, initMsg)
	return
}

func ETLStop(baseParams BaseParams, id string) (err error) {
	return etlPostAction(baseParams, id, cmn.ETLStop)
}

func ETLStart(baseParams BaseParams, id string) (err error) {
	return etlPostAction(baseParams, id, cmn.ETLStart)
}

func etlPostAction(baseParams BaseParams, id, action string) (err error) {
	baseParams.Method = http.MethodPost
	reqParams := allocRp()
	{
		reqParams.BaseParams = baseParams
		reqParams.Path = cmn.URLPathETL.Join(id, action)
	}
	err = reqParams.DoHTTPRequest()
	freeRp(reqParams)
	return err
}

// TODO: "if query has UUID then the request is ETL" is not good enough. Add ETL-specific
//       query param and change the examples/docs (!4455)
func ETLObject(baseParams BaseParams, id string, bck cmn.Bck, objName string, w io.Writer) (err error) {
	_, err = GetObject(baseParams, bck, objName, GetObjectInput{
		Writer: w,
		Query:  url.Values{cmn.QparamUUID: []string{id}},
	})
	return
}

func ETLBucket(baseParams BaseParams, fromBck, toBck cmn.Bck, bckMsg *cmn.TCBMsg) (xactID string, err error) {
	if err = toBck.Validate(); err != nil {
		return
	}
	baseParams.Method = http.MethodPost
	q := cmn.AddBckToQuery(nil, fromBck)
	_ = cmn.AddBckUnameToQuery(q, toBck, cmn.QparamBucketTo)
	reqParams := allocRp()
	{
		reqParams.BaseParams = baseParams
		reqParams.Path = cmn.URLPathBuckets.Join(fromBck.Name)
		reqParams.Body = cos.MustMarshal(cmn.ActionMsg{Action: cmn.ActETLBck, Value: bckMsg})
		reqParams.Header = http.Header{cmn.HdrContentType: []string{cmn.ContentJSON}}
		reqParams.Query = q
	}
	err = reqParams.DoHTTPReqResp(&xactID)
	freeRp(reqParams)
	return
}
