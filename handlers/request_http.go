package handlers

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"math/rand"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/nicholasjackson/fake-service/client"
	"github.com/nicholasjackson/fake-service/errors"
	"github.com/nicholasjackson/fake-service/load"
	"github.com/nicholasjackson/fake-service/logging"
	"github.com/nicholasjackson/fake-service/response"
	"github.com/nicholasjackson/fake-service/timing"
	"github.com/nicholasjackson/fake-service/worker"
)

// done is a message sent when an upstream worker has completed
type done struct {
	uri  string
	data []byte
}

// Request handles inbound requests and makes any necessary upstream calls
type Request struct {
	// name of the service
	name string
	// message to return to caller
	message            string
	externalServiceURL string
	duration           *timing.RequestDuration
	upstreamURIs       []string
	workerCount        int
	defaultClient      client.HTTP
	grpcClients        map[string]client.GRPC
	errorInjector      *errors.Injector
	loadGenerator      *load.Generator
	log                *logging.Logger
}

// NewRequest creates a new request handler
func NewRequest(
	name, message string, externalServiceURL string,
	duration *timing.RequestDuration,
	upstreamURIs []string,
	workerCount int,
	defaultClient client.HTTP,
	grpcClients map[string]client.GRPC,
	errorInjector *errors.Injector,
	loadGenerator *load.Generator,
	log *logging.Logger,
) *Request {

	return &Request{
		name:               name,
		message:            message,
		externalServiceURL: externalServiceURL,
		duration:           duration,
		upstreamURIs:       upstreamURIs,
		workerCount:        workerCount,
		defaultClient:      defaultClient,
		grpcClients:        grpcClients,
		errorInjector:      errorInjector,
		loadGenerator:      loadGenerator,
		log:                log,
	}
}

// Handle the request and call the upstream servers
func (rq *Request) Handle(rw http.ResponseWriter, r *http.Request) {
	// generate 100% CPU load for service
	finished := rq.loadGenerator.Generate()
	defer finished()

	// start timing the service this is used later for the total request time
	ts := time.Now()

	// log start request
	hq := rq.log.HandleHTTPRequest(r)
	defer hq.Finished()

	resp := &response.Response{}
	resp.Name = rq.name
	resp.Type = "HTTP"
	resp.URI = r.URL.String()
	resp.IPAddresses = getIPInfo()

	// are we injecting errors, if so return the error
	if er := rq.errorInjector.Do(); er != nil {
		resp.Code = er.Code
		resp.Error = er.Error.Error()

		// log the error response
		hq.SetError(er.Error)
		hq.SetMetadata("response", strconv.Itoa(er.Code))

		rw.WriteHeader(er.Code)
		rw.Write([]byte(resp.ToJSON()))
		return
	}

	// if we need to create upstream requests create a worker pool
	var upstreamError error
	if len(rq.upstreamURIs) > 0 {
		wp := worker.New(rq.workerCount, func(uri string) (*response.Response, error) {
			if strings.HasPrefix(uri, "http://") {
				return workerHTTP(hq.Span.Context(), uri, rq.defaultClient, r, rq.log)
			}

			return workerGRPC(hq.Span.Context(), uri, rq.grpcClients, rq.log)
		})

		err := wp.Do(rq.upstreamURIs)

		if err != nil {
			upstreamError = err
		}

		for _, v := range wp.Responses() {
			resp.AppendUpstream(v.URI, *v.Response)
		}
	}

	// service time is equal to the randomised time - the current time take
	d := rq.duration.Calculate()
	et := time.Now().Sub(ts)
	rd := d - et

	// set the start end end time

	if upstreamError != nil {
		rw.WriteHeader(http.StatusInternalServerError)
		resp.Code = http.StatusInternalServerError

		// log error
		hq.SetMetadata("response", strconv.Itoa(http.StatusInternalServerError))
		hq.SetError(upstreamError)
	} else {
		// randomize the time the request takes if no error
		lp := rq.log.SleepService(hq.Span, rd)

		if rd > 0 {
			time.Sleep(rd)
		}

		lp.Finished()
		resp.Code = http.StatusOK

		// log response code
		hq.SetMetadata("response", strconv.Itoa(http.StatusOK))
	}

	// caclulcate total elapsed time including delay
	te := time.Now()
	et = te.Sub(ts)

	resp.StartTime = ts.Format(timeFormat)
	resp.EndTime = te.Format(timeFormat)
	resp.Duration = et.String()

	// Update message by adding response from the external service
	updatedMessage := fmt.Sprintf("%s + %s", rq.message, getExternalServiceMessage(rq.externalServiceURL))

	// add the response body
	if strings.HasPrefix(rq.message, "{") {
		resp.Body = json.RawMessage(updatedMessage)
	} else {
		resp.Body = json.RawMessage(fmt.Sprintf(`"%s"`, updatedMessage))
	}

	rw.Write([]byte(resp.ToJSON()))
}

type userdata struct {
	UserId int    `json:"userId"`
	Id     int    `json:"id"`
	Title  string `json:"title"`
	Body   string `json:"body"`
}

// return the external service message
func getExternalServiceMessage(externalServiceURL string) string {
	// random generate a number from 1 to 100
	randomNum := rand.Intn(100-1) + 1
	path := externalServiceURL + "?id=" + strconv.Itoa(randomNum)
	var u []userdata

	// connect to external service to get a response.
	resp, err := http.Get(path)
	if err != nil {
		fmt.Println(err)
		return "Error communicating to the external service: " + err.Error()
	}
	defer resp.Body.Close()

	body, err := ioutil.ReadAll(resp.Body)

	if err != nil {
		return "Error communicating to the external service: " + err.Error()
	}

	err = json.Unmarshal(body, &u)
	if err != nil {
		return "Error communicating to the external service: " + err.Error()
	}
	if len(u) > 0 {
		return "History: " + strconv.Itoa(u[0].Id) + " Title: " + u[0].Title + " Body: " + u[0].Title
	} else {
		return "Error communicating to the external service: " + err.Error()
	}
}
