/*
 * Copyright 2017-2018 Dgraph Labs, Inc. and Contributors
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package alpha

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/dgraph-io/dgo/protos/api"
	"github.com/golang/glog"
	"google.golang.org/grpc/metadata"

	"github.com/dgraph-io/dgraph/edgraph"
	"github.com/dgraph-io/dgraph/gql"
	"github.com/dgraph-io/dgraph/query"
	"github.com/dgraph-io/dgraph/worker"
	"github.com/dgraph-io/dgraph/x"
)

func allowed(method string) bool {
	return method == http.MethodPost || method == http.MethodPut
}

func extractStartTs(urlPath string) (uint64, error) {
	params := strings.Split(strings.TrimPrefix(urlPath, "/"), "/")

	switch l := len(params); l {
	case 1:
		// When startTs is not supplied. /query or /mutate
		return 0, nil
	case 2:
		ts, err := strconv.ParseUint(params[1], 0, 64)
		if err != nil {
			return 0, fmt.Errorf("Error: %+v while parsing StartTs path parameter as uint64", err)
		}
		return ts, nil
	default:
		return 0, x.Errorf("Incorrect no. of path parameters. Expected 1 or 2. Got: %+v", l)
	}
}

/*// Common functionality for these request handlers. Returns true if the request is completely
// handled and nothing further needs to be done.
func commonHandlerOld(w http.ResponseWriter, r *http.Request) bool {
	// Do these requests really need CORS headers? Doesn't seem like it, but they are probably
	// harmless aside from the extra size they add to each response.
	x.AddCorsHeaders(w)

	if r.Method == "OPTIONS" {
		return true
	} else if !allowed(r.Method) {
		w.WriteHeader(http.StatusBadRequest)
		x.SetStatus(w, x.ErrorInvalidMethod, "Invalid method")
		return true
	}

	w.Header().Set("Content-Type", "application/json")

	return false
}

// Common functionality for these request handlers. Returns a nil writer if the request is
// completely handled and nothing further needs to be done.
func commonHandlerOld2(w http.ResponseWriter, r *http.Request) (io.Writer, io.Reader) {
	// Do these requests really need CORS headers? Doesn't seem like it, but they are probably
	// harmless aside from the extra size they add to each response.
	x.AddCorsHeaders(w)

	if r.Method == "OPTIONS" {
		return nil, nil
	} else if !allowed(r.Method) {
		w.WriteHeader(http.StatusBadRequest)
		x.SetStatus(w, x.ErrorInvalidMethod, "Invalid method")
		return nil, nil
	}

	var err error
	rd := r.Body
	if enc := r.Header.Get("Content-Encoding"); enc != "" && enc != "identity" {
		if enc == "gzip" {
			rd, err = gzip.NewReader(rd)
			if err != nil {
				x.SetStatus(w, x.Error, "Unable to create decompressor")
				return nil, nil
			}
		} else {
			x.SetStatus(w, x.ErrorInvalidRequest, "Unsupported content encoding")
			return nil, nil
		}
	}

	wr := w

	w.Header().Set("Content-Type", "application/json")

	return wr, rd
}*/

type streams struct {
	in   io.Reader
	out  io.Writer
	clos []io.Closer
}

func (s *streams) close() {
	for _, s := range s.clos {
		_ = s.Close()
	}
}

// Common functionality for these request handlers. Returns nil writer if the request is
// completely handled here and nothing further needs to be done.
func commonHandler(w http.ResponseWriter, r *http.Request) *streams {
	// Do these requests really need CORS headers? Doesn't seem like it, but they are probably
	// harmless aside from the extra size they add to each response.
	x.AddCorsHeaders(w)

	if r.Method == "OPTIONS" {
		return nil
	} else if !allowed(r.Method) {
		w.WriteHeader(http.StatusBadRequest)
		x.SetStatus(w, x.ErrorInvalidMethod, "Invalid method")
		return nil
	}

	// These are the readers and writers the caller should use for the request and response
	// body in order to handle compressed and uncompressed bodies transparently.
	s := &streams{in: r.Body, out: w}

	if enc := r.Header.Get("Content-Encoding"); enc != "" && enc != "identity" {
		if enc == "gzip" {
			gz, err := gzip.NewReader(s.in)
			if err != nil {
				x.SetStatus(w, x.Error, "Unable to create decompressor")
				return nil
			}
			s.clos = append(s.clos, gz)
		} else {
			x.SetStatus(w, x.ErrorInvalidRequest, "Unsupported content encoding")
			return nil
		}
	}

	if strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
		w.Header().Set("Content-Encoding", "gzip")
		gz := gzip.NewWriter(w)
		s.clos = append(s.clos, gz)
	}

	w.Header().Set("Content-Type", "application/json")

	return s
}

// This method should just build the request and proxy it to the Query method of dgraph.Server.
// It can then encode the response as appropriate before sending it back to the user.
func queryHandler(w http.ResponseWriter, r *http.Request) {
	s := commonHandler(w, r)
	if s == nil {
		return
	}
	defer s.close()

	req := api.Request{}
	ts, err := extractStartTs(r.URL.Path)
	if err != nil {
		x.SetStatus(w, err.Error(), x.ErrorInvalidRequest)
		return
	}
	req.StartTs = ts

	if vars := r.Header.Get("X-Dgraph-Vars"); vars != "" {
		req.Vars = map[string]string{}
		if err := json.Unmarshal([]byte(vars), &req.Vars); err != nil {
			x.SetStatus(w, x.ErrorInvalidRequest,
				"Error while unmarshalling Vars header into map")
			return
		}
	}

	q, err := ioutil.ReadAll(s.in)
	if err != nil {
		x.SetStatus(w, x.ErrorInvalidRequest, err.Error())
		return
	}
	req.Query = string(q)

	d := r.URL.Query().Get("debug")
	ctx := context.WithValue(context.Background(), "debug", d)

	// If ro is set, run this as a readonly query.
	if ro := r.URL.Query().Get("ro"); len(ro) > 0 && req.StartTs == 0 {
		if ro == "true" || ro == "1" {
			req.ReadOnly = true
		}
	}

	// Core processing happens here.
	resp, err := (&edgraph.Server{}).Query(ctx, &req)
	if err != nil {
		x.SetStatusWithData(w, x.ErrorInvalidRequest, err.Error())
		return
	}

	var out bytes.Buffer
	writeEntry := func(key string, js []byte) {
		out.WriteRune('"')
		out.WriteString(key)
		out.WriteRune('"')
		out.WriteRune(':')
		out.Write(js)
	}

	e := query.Extensions{
		Txn:     resp.Txn,
		Latency: resp.Latency,
	}
	js, err := json.Marshal(e)
	if err != nil {
		x.SetStatusWithData(w, x.Error, err.Error())
		return
	}
	out.WriteRune('{')
	writeEntry("extensions", js)
	out.WriteRune(',')

	// User can either ask for schema or have a query.
	if len(resp.Schema) > 0 {
		sort.Slice(resp.Schema, func(i, j int) bool {
			return resp.Schema[i].Predicate < resp.Schema[j].Predicate
		})
		js, err := json.Marshal(resp.Schema)
		if err != nil {
			x.SetStatusWithData(w, x.Error, "Unable to marshal schema")
			return
		}

		writeEntry("data", nil)
		out.WriteRune('{')
		writeEntry("schema", js)
		out.WriteRune('}')
	} else {
		writeEntry("data", resp.Json)
	}
	out.WriteRune('}')
	s.out.Write(out.Bytes())
}

func mutationHandler(w http.ResponseWriter, r *http.Request) {
	s := commonHandler(w, r)
	if s == nil {
		return
	}
	defer s.close()

	m, err := ioutil.ReadAll(s.in)
	if err != nil {
		x.SetStatus(w, x.ErrorInvalidRequest, err.Error())
		return
	}

	parseStart := time.Now()

	var mu *api.Mutation
	if mType := r.Header.Get("X-Dgraph-MutationType"); mType == "json" {
		// Parse JSON.
		ms := make(map[string]*skipJSONUnmarshal)
		err := json.Unmarshal(m, &ms)
		if err != nil {
			x.SetStatus(w, x.ErrorInvalidRequest, err.Error())
			return
		}

		mu = &api.Mutation{}
		if setJSON, ok := ms["set"]; ok && setJSON != nil {
			mu.SetJson = setJSON.bs
		}
		if delJSON, ok := ms["delete"]; ok && delJSON != nil {
			mu.DeleteJson = delJSON.bs
		}
	} else {
		// Parse NQuads.
		mu, err = gql.ParseMutation(string(m))
		if err != nil {
			x.SetStatus(w, x.ErrorInvalidRequest, err.Error())
			return
		}
	}

	parseEnd := time.Now()

	// Maybe rename it so that default is CommitNow.
	commit := r.Header.Get("X-Dgraph-CommitNow")
	if commit != "" {
		c, err := strconv.ParseBool(commit)
		if err != nil {
			x.SetStatus(w, x.ErrorInvalidRequest,
				"Error while parsing Commit header as bool")
			return
		}
		mu.CommitNow = c
	}

	ts, err := extractStartTs(r.URL.Path)
	if err != nil {
		x.SetStatus(w, err.Error(), x.ErrorInvalidRequest)
		return
	}
	mu.StartTs = ts

	resp, err := (&edgraph.Server{}).Mutate(context.Background(), mu)
	if err != nil {
		x.SetStatusWithData(w, x.ErrorInvalidRequest, err.Error())
		return
	}

	resp.Latency.ParsingNs = uint64(parseEnd.Sub(parseStart).Nanoseconds())
	e := query.Extensions{
		Txn:     resp.Context,
		Latency: resp.Latency,
	}

	// Don't send keys array which is part of txn context if its commit immediately.
	if mu.CommitNow {
		e.Txn.Keys = e.Txn.Keys[:0]
	}

	response := map[string]interface{}{}
	response["extensions"] = e
	mp := map[string]interface{}{}
	mp["code"] = x.Success
	mp["message"] = "Done"
	mp["uids"] = resp.Uids
	response["data"] = mp

	js, err := json.Marshal(response)
	if err != nil {
		x.SetStatusWithData(w, x.Error, err.Error())
		return
	}
	s.out.Write(js)
}

func commitHandler(w http.ResponseWriter, r *http.Request) {
	s := commonHandler(w, r)
	if s == nil {
		return
	}
	defer s.close()

	resp := &api.Assigned{}
	tc := &api.TxnContext{}
	resp.Context = tc

	ts, err := extractStartTs(r.URL.Path)
	if err != nil {
		x.SetStatus(w, err.Error(), x.ErrorInvalidRequest)
		return
	}

	if ts == 0 {
		x.SetStatus(w, x.ErrorInvalidRequest,
			"StartTs path parameter is mandatory while trying to commit")
		return
	}
	tc.StartTs = ts

	// Keys are sent as an array in the body.
	keys, err := ioutil.ReadAll(s.in)
	if err != nil {
		x.SetStatus(w, x.ErrorInvalidRequest, err.Error())
		return
	}

	var encodedKeys []string
	if err := json.Unmarshal([]byte(keys), &encodedKeys); err != nil {
		x.SetStatus(w, x.ErrorInvalidRequest,
			"Error while unmarshalling keys header into array")
		return
	}

	tc.Keys = encodedKeys

	cts, err := worker.CommitOverNetwork(context.Background(), tc)
	if err != nil {
		x.SetStatus(w, x.Error, err.Error())
		return
	}
	resp.Context.CommitTs = cts

	e := query.Extensions{
		Txn: resp.Context,
	}
	e.Txn.Keys = e.Txn.Keys[:0]
	response := map[string]interface{}{}
	response["extensions"] = e
	mp := map[string]interface{}{}
	mp["code"] = x.Success
	mp["message"] = "Done"
	response["data"] = mp

	js, err := json.Marshal(response)
	if err != nil {
		x.SetStatusWithData(w, x.Error, err.Error())
		return
	}
	s.out.Write(js)
}

func abortHandler(w http.ResponseWriter, r *http.Request) {
	s := commonHandler(w, r)
	if s == nil {
		return
	}
	defer s.close()

	resp := &api.Assigned{}
	tc := &api.TxnContext{}
	resp.Context = tc

	ts, err := extractStartTs(r.URL.Path)
	if err != nil {
		x.SetStatus(w, err.Error(), x.ErrorInvalidRequest)
		return
	}

	if ts == 0 {
		x.SetStatus(w, x.ErrorInvalidRequest,
			"StartTs path parameter is mandatory while trying to abort.")
		return
	}
	tc.StartTs = ts
	tc.Aborted = true

	_, aerr := worker.CommitOverNetwork(context.Background(), tc)
	if aerr != nil {
		x.SetStatus(w, x.Error, aerr.Error())
		return
	}

	response := map[string]interface{}{}
	response["code"] = x.Success
	response["message"] = "Done"

	js, err := json.Marshal(response)
	if err != nil {
		x.SetStatusWithData(w, x.Error, err.Error())
		return
	}
	s.out.Write(js)
}

func alterHandler(w http.ResponseWriter, r *http.Request) {
	s := commonHandler(w, r)
	if s == nil {
		return
	}
	defer s.close()

	op := &api.Operation{}

	b, err := ioutil.ReadAll(s.in)
	if err != nil {
		x.SetStatus(w, x.ErrorInvalidRequest, err.Error())
		return
	}

	err = json.Unmarshal(b, &op)
	if err != nil {
		op.Schema = string(b)
	}

	glog.Infof("Got alter request via HTTP from %s\n", r.RemoteAddr)
	fwd := r.Header.Get("X-Forwarded-For")
	if len(fwd) > 0 {
		glog.Infof("The alter request is forwarded by %s\n", fwd)
	}

	md := metadata.New(nil)
	// Pass in an auth token, if present.
	md.Append("auth-token", r.Header.Get("X-Dgraph-AuthToken"))
	ctx := metadata.NewIncomingContext(context.Background(), md)
	if _, err = (&edgraph.Server{}).Alter(ctx, op); err != nil {
		x.SetStatus(w, x.Error, err.Error())
		return
	}

	res := map[string]interface{}{}
	data := map[string]interface{}{}
	data["code"] = x.Success
	data["message"] = "Done"
	res["data"] = data

	js, err := json.Marshal(res)
	if err != nil {
		x.SetStatus(w, x.Error, err.Error())
		return
	}
	s.out.Write(js)
}

// skipJSONUnmarshal stores the raw bytes as is while JSON unmarshaling.
type skipJSONUnmarshal struct {
	bs []byte
}

func (sju *skipJSONUnmarshal) UnmarshalJSON(bs []byte) error {
	sju.bs = bs
	return nil
}
