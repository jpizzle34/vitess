/*
   Copyright 2014 Outbrain Inc.

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package http

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-martini/martini"
	"github.com/martini-contrib/auth"
	"github.com/martini-contrib/render"

	"vitess.io/vitess/go/vt/orchestrator/external/golib/log"
	"vitess.io/vitess/go/vt/orchestrator/external/golib/util"

	"vitess.io/vitess/go/vt/orchestrator/collection"
	"vitess.io/vitess/go/vt/orchestrator/config"
	"vitess.io/vitess/go/vt/orchestrator/discovery"
	"vitess.io/vitess/go/vt/orchestrator/inst"
	"vitess.io/vitess/go/vt/orchestrator/logic"
	"vitess.io/vitess/go/vt/orchestrator/metrics/query"
	"vitess.io/vitess/go/vt/orchestrator/process"
	orcraft "vitess.io/vitess/go/vt/orchestrator/raft"
	"vitess.io/vitess/go/vt/vtctl/reparentutil/promotionrule"
)

// APIResponseCode is an OK/ERROR response code
type APIResponseCode int

const (
	ERROR APIResponseCode = iota
	OK
)

var registeredPaths = []string{}
var emptyInstanceKey inst.InstanceKey

func (this *APIResponseCode) MarshalJSON() ([]byte, error) {
	return json.Marshal(this.String())
}

func (this *APIResponseCode) String() string {
	switch *this {
	case ERROR:
		return "ERROR"
	case OK:
		return "OK"
	}
	return "unknown"
}

// HttpStatus returns the respective HTTP status for this response
func (this *APIResponseCode) HttpStatus() int {
	switch *this {
	case ERROR:
		return http.StatusInternalServerError
	case OK:
		return http.StatusOK
	}
	return http.StatusNotImplemented
}

// APIResponse is a response returned as JSON to various requests.
type APIResponse struct {
	Code    APIResponseCode
	Message string
	Details interface{}
}

func Respond(r render.Render, apiResponse *APIResponse) {
	r.JSON(apiResponse.Code.HttpStatus(), apiResponse)
}

type HttpAPI struct {
	URLPrefix string
}

var API HttpAPI = HttpAPI{}
var discoveryMetrics = collection.CreateOrReturnCollection("DISCOVERY_METRICS")
var queryMetrics = collection.CreateOrReturnCollection("BACKEND_WRITES")
var writeBufferMetrics = collection.CreateOrReturnCollection("WRITE_BUFFER")

func (this *HttpAPI) getInstanceKeyInternal(host string, port string, resolve bool) (inst.InstanceKey, error) {
	var instanceKey *inst.InstanceKey
	var err error
	if resolve {
		instanceKey, err = inst.NewResolveInstanceKeyStrings(host, port)
	} else {
		instanceKey, err = inst.NewRawInstanceKeyStrings(host, port)
	}
	if err != nil {
		return emptyInstanceKey, err
	}
	instanceKey, err = inst.FigureInstanceKey(instanceKey, nil)
	if err != nil {
		return emptyInstanceKey, err
	}
	if instanceKey == nil {
		return emptyInstanceKey, fmt.Errorf("Unexpected nil instanceKey in getInstanceKeyInternal(%+v, %+v, %+v)", host, port, resolve)
	}
	return *instanceKey, nil
}

func (this *HttpAPI) getInstanceKey(host string, port string) (inst.InstanceKey, error) {
	return this.getInstanceKeyInternal(host, port, true)
}

func (this *HttpAPI) getNoResolveInstanceKey(host string, port string) (inst.InstanceKey, error) {
	return this.getInstanceKeyInternal(host, port, false)
}

func getTag(params martini.Params, req *http.Request) (tag *inst.Tag, err error) {
	tagString := req.URL.Query().Get("tag")
	if tagString != "" {
		return inst.ParseTag(tagString)
	}
	return inst.NewTag(params["tagName"], params["tagValue"])
}

func (this *HttpAPI) getBinlogCoordinates(logFile string, logPos string) (inst.BinlogCoordinates, error) {
	coordinates := inst.BinlogCoordinates{LogFile: logFile}
	var err error
	if coordinates.LogPos, err = strconv.ParseInt(logPos, 10, 0); err != nil {
		return coordinates, fmt.Errorf("Invalid logPos: %s", logPos)
	}

	return coordinates, err
}

// InstanceReplicas lists all replicas of given instance
func (this *HttpAPI) InstanceReplicas(params martini.Params, r render.Render, req *http.Request) {
	instanceKey, err := this.getInstanceKey(params["host"], params["port"])

	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: err.Error()})
		return
	}
	replicas, err := inst.ReadReplicaInstances(&instanceKey)

	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: fmt.Sprintf("Cannot read instance: %+v", instanceKey)})
		return
	}
	r.JSON(http.StatusOK, replicas)
}

// Instance reads and returns an instance's details.
func (this *HttpAPI) Instance(params martini.Params, r render.Render, req *http.Request) {
	instanceKey, err := this.getInstanceKey(params["host"], params["port"])

	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: err.Error()})
		return
	}
	instance, found, err := inst.ReadInstance(&instanceKey)
	if (!found) || (err != nil) {
		Respond(r, &APIResponse{Code: ERROR, Message: fmt.Sprintf("Cannot read instance: %+v", instanceKey)})
		return
	}
	r.JSON(http.StatusOK, instance)
}

// AsyncDiscover issues an asynchronous read on an instance. This is
// useful for bulk loads of a new set of instances and will not block
// if the instance is slow to respond or not reachable.
func (this *HttpAPI) AsyncDiscover(params martini.Params, r render.Render, req *http.Request, user auth.User) {
	if !isAuthorizedForAction(req, user) {
		Respond(r, &APIResponse{Code: ERROR, Message: "Unauthorized"})
		return
	}
	instanceKey, err := this.getInstanceKey(params["host"], params["port"])
	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: err.Error()})
		return
	}
	go this.Discover(params, r, req, user)

	Respond(r, &APIResponse{Code: OK, Message: fmt.Sprintf("Asynchronous discovery initiated for Instance: %+v", instanceKey)})
}

// Discover issues a synchronous read on an instance
func (this *HttpAPI) Discover(params martini.Params, r render.Render, req *http.Request, user auth.User) {
	if !isAuthorizedForAction(req, user) {
		Respond(r, &APIResponse{Code: ERROR, Message: "Unauthorized"})
		return
	}
	instanceKey, err := this.getInstanceKey(params["host"], params["port"])
	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: err.Error()})
		return
	}
	instance, err := inst.ReadTopologyInstance(&instanceKey)
	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: err.Error()})
		return
	}

	if orcraft.IsRaftEnabled() {
		orcraft.PublishCommand("discover", instanceKey)
	} else {
		logic.DiscoverInstance(instanceKey, false /* forceDiscovery */)
	}

	Respond(r, &APIResponse{Code: OK, Message: fmt.Sprintf("Instance discovered: %+v", instance.Key), Details: instance})
}

// Refresh synchronuously re-reads a topology instance
func (this *HttpAPI) Refresh(params martini.Params, r render.Render, req *http.Request, user auth.User) {
	if !isAuthorizedForAction(req, user) {
		Respond(r, &APIResponse{Code: ERROR, Message: "Unauthorized"})
		return
	}
	instanceKey, err := this.getInstanceKey(params["host"], params["port"])

	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: err.Error()})
		return
	}

	_, err = inst.RefreshTopologyInstance(&instanceKey)
	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: err.Error()})
		return
	}

	Respond(r, &APIResponse{Code: OK, Message: fmt.Sprintf("Instance refreshed: %+v", instanceKey), Details: instanceKey})
}

// Forget removes an instance entry fro backend database
func (this *HttpAPI) Forget(params martini.Params, r render.Render, req *http.Request, user auth.User) {
	if !isAuthorizedForAction(req, user) {
		Respond(r, &APIResponse{Code: ERROR, Message: "Unauthorized"})
		return
	}
	instanceKey, err := this.getNoResolveInstanceKey(params["host"], params["port"])
	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: err.Error()})
		return
	}

	if orcraft.IsRaftEnabled() {
		_, err = orcraft.PublishCommand("forget", instanceKey)
	} else {
		err = inst.ForgetInstance(&instanceKey)
	}
	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: err.Error()})
		return
	}
	Respond(r, &APIResponse{Code: OK, Message: fmt.Sprintf("Instance forgotten: %+v", instanceKey), Details: instanceKey})
}

// ForgetCluster forgets all instacnes of a cluster
func (this *HttpAPI) ForgetCluster(params martini.Params, r render.Render, req *http.Request, user auth.User) {
	if !isAuthorizedForAction(req, user) {
		Respond(r, &APIResponse{Code: ERROR, Message: "Unauthorized"})
		return
	}
	clusterName, err := figureClusterName(getClusterHint(params))
	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: fmt.Sprintf("%+v", err)})
		return
	}

	if orcraft.IsRaftEnabled() {
		orcraft.PublishCommand("forget-cluster", clusterName)
	} else {
		inst.ForgetCluster(clusterName)
	}
	Respond(r, &APIResponse{Code: OK, Message: fmt.Sprintf("Cluster forgotten: %+v", clusterName)})
}

// Resolve tries to resolve hostname and then checks to see if port is open on that host.
func (this *HttpAPI) Resolve(params martini.Params, r render.Render, req *http.Request) {
	instanceKey, err := this.getInstanceKey(params["host"], params["port"])

	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: err.Error()})
		return
	}

	if conn, err := net.Dial("tcp", instanceKey.DisplayString()); err == nil {
		conn.Close()
	} else {
		Respond(r, &APIResponse{Code: ERROR, Message: err.Error()})
		return
	}

	Respond(r, &APIResponse{Code: OK, Message: "Instance resolved", Details: instanceKey})
}

// BeginMaintenance begins maintenance mode for given instance
func (this *HttpAPI) BeginMaintenance(params martini.Params, r render.Render, req *http.Request, user auth.User) {
	if !isAuthorizedForAction(req, user) {
		Respond(r, &APIResponse{Code: ERROR, Message: "Unauthorized"})
		return
	}
	instanceKey, err := this.getInstanceKey(params["host"], params["port"])

	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: err.Error()})
		return
	}
	key, err := inst.BeginBoundedMaintenance(&instanceKey, params["owner"], params["reason"], 0, true)
	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: err.Error(), Details: key})
		return
	}

	Respond(r, &APIResponse{Code: OK, Message: fmt.Sprintf("Maintenance begun: %+v", instanceKey), Details: instanceKey})
}

// EndMaintenance terminates maintenance mode
func (this *HttpAPI) EndMaintenance(params martini.Params, r render.Render, req *http.Request, user auth.User) {
	if !isAuthorizedForAction(req, user) {
		Respond(r, &APIResponse{Code: ERROR, Message: "Unauthorized"})
		return
	}
	maintenanceKey, err := strconv.ParseInt(params["maintenanceKey"], 10, 0)
	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: err.Error()})
		return
	}
	_, err = inst.EndMaintenance(maintenanceKey)
	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: err.Error()})
		return
	}

	Respond(r, &APIResponse{Code: OK, Message: fmt.Sprintf("Maintenance ended: %+v", maintenanceKey), Details: maintenanceKey})
}

// EndMaintenanceByInstanceKey terminates maintenance mode for given instance
func (this *HttpAPI) EndMaintenanceByInstanceKey(params martini.Params, r render.Render, req *http.Request, user auth.User) {
	if !isAuthorizedForAction(req, user) {
		Respond(r, &APIResponse{Code: ERROR, Message: "Unauthorized"})
		return
	}
	instanceKey, err := this.getInstanceKey(params["host"], params["port"])

	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: err.Error()})
		return
	}
	_, err = inst.EndMaintenanceByInstanceKey(&instanceKey)
	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: err.Error()})
		return
	}

	Respond(r, &APIResponse{Code: OK, Message: fmt.Sprintf("Maintenance ended: %+v", instanceKey), Details: instanceKey})
}

// EndMaintenanceByInstanceKey terminates maintenance mode for given instance
func (this *HttpAPI) InMaintenance(params martini.Params, r render.Render, req *http.Request, user auth.User) {
	instanceKey, err := this.getInstanceKey(params["host"], params["port"])
	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: err.Error()})
		return
	}
	inMaintenance, err := inst.InMaintenance(&instanceKey)
	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: err.Error()})
		return
	}
	responseDetails := ""
	if inMaintenance {
		responseDetails = instanceKey.StringCode()
	}
	Respond(r, &APIResponse{Code: OK, Message: fmt.Sprintf("%+v", inMaintenance), Details: responseDetails})
}

// Maintenance provides list of instance under active maintenance
func (this *HttpAPI) Maintenance(params martini.Params, r render.Render, req *http.Request) {
	maintenanceList, err := inst.ReadActiveMaintenance()

	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: fmt.Sprintf("%+v", err)})
		return
	}

	r.JSON(http.StatusOK, maintenanceList)
}

// BeginDowntime sets a downtime flag with default duration
func (this *HttpAPI) BeginDowntime(params martini.Params, r render.Render, req *http.Request, user auth.User) {
	if !isAuthorizedForAction(req, user) {
		Respond(r, &APIResponse{Code: ERROR, Message: "Unauthorized"})
		return
	}
	instanceKey, err := this.getInstanceKey(params["host"], params["port"])

	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: err.Error()})
		return
	}

	var durationSeconds int = 0
	if params["duration"] != "" {
		durationSeconds, err = util.SimpleTimeToSeconds(params["duration"])
		if durationSeconds < 0 {
			err = fmt.Errorf("Duration value must be non-negative. Given value: %d", durationSeconds)
		}
		if err != nil {
			Respond(r, &APIResponse{Code: ERROR, Message: err.Error()})
			return
		}
	}
	duration := time.Duration(durationSeconds) * time.Second
	downtime := inst.NewDowntime(&instanceKey, params["owner"], params["reason"], duration)
	if orcraft.IsRaftEnabled() {
		_, err = orcraft.PublishCommand("begin-downtime", downtime)
	} else {
		err = inst.BeginDowntime(downtime)
	}

	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: err.Error(), Details: instanceKey})
		return
	}

	Respond(r, &APIResponse{Code: OK, Message: fmt.Sprintf("Downtime begun: %+v", instanceKey), Details: instanceKey})
}

// EndDowntime terminates downtime (removes downtime flag) for an instance
func (this *HttpAPI) EndDowntime(params martini.Params, r render.Render, req *http.Request, user auth.User) {
	if !isAuthorizedForAction(req, user) {
		Respond(r, &APIResponse{Code: ERROR, Message: "Unauthorized"})
		return
	}
	instanceKey, err := this.getInstanceKey(params["host"], params["port"])

	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: err.Error()})
		return
	}
	if orcraft.IsRaftEnabled() {
		_, err = orcraft.PublishCommand("end-downtime", instanceKey)
	} else {
		_, err = inst.EndDowntime(&instanceKey)
	}
	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: err.Error()})
		return
	}

	Respond(r, &APIResponse{Code: OK, Message: fmt.Sprintf("Downtime ended: %+v", instanceKey), Details: instanceKey})
}

// MoveUp attempts to move an instance up the topology
func (this *HttpAPI) MoveUp(params martini.Params, r render.Render, req *http.Request, user auth.User) {
	if !isAuthorizedForAction(req, user) {
		Respond(r, &APIResponse{Code: ERROR, Message: "Unauthorized"})
		return
	}
	instanceKey, err := this.getInstanceKey(params["host"], params["port"])

	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: err.Error()})
		return
	}
	instance, err := inst.MoveUp(&instanceKey)
	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: err.Error()})
		return
	}

	Respond(r, &APIResponse{Code: OK, Message: fmt.Sprintf("Instance %+v moved up", instanceKey), Details: instance})
}

// MoveUpReplicas attempts to move up all replicas of an instance
func (this *HttpAPI) MoveUpReplicas(params martini.Params, r render.Render, req *http.Request, user auth.User) {
	if !isAuthorizedForAction(req, user) {
		Respond(r, &APIResponse{Code: ERROR, Message: "Unauthorized"})
		return
	}
	instanceKey, err := this.getInstanceKey(params["host"], params["port"])
	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: err.Error()})
		return
	}

	replicas, newPrimary, err, errs := inst.MoveUpReplicas(&instanceKey, req.URL.Query().Get("pattern"))
	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: err.Error()})
		return
	}

	Respond(r, &APIResponse{Code: OK, Message: fmt.Sprintf("Moved up %d replicas of %+v below %+v; %d errors: %+v", len(replicas), instanceKey, newPrimary.Key, len(errs), errs), Details: replicas})
}

// Repoint positiones a replica under another (or same) primary with exact same coordinates.
// Useful for binlog servers
func (this *HttpAPI) Repoint(params martini.Params, r render.Render, req *http.Request, user auth.User) {
	if !isAuthorizedForAction(req, user) {
		Respond(r, &APIResponse{Code: ERROR, Message: "Unauthorized"})
		return
	}
	instanceKey, err := this.getInstanceKey(params["host"], params["port"])
	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: err.Error()})
		return
	}
	belowKey, err := this.getInstanceKey(params["belowHost"], params["belowPort"])
	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: err.Error()})
		return
	}

	instance, err := inst.Repoint(&instanceKey, &belowKey, inst.GTIDHintNeutral)
	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: err.Error()})
		return
	}

	Respond(r, &APIResponse{Code: OK, Message: fmt.Sprintf("Instance %+v repointed below %+v", instanceKey, belowKey), Details: instance})
}

// MoveUpReplicas attempts to move up all replicas of an instance
func (this *HttpAPI) RepointReplicas(params martini.Params, r render.Render, req *http.Request, user auth.User) {
	if !isAuthorizedForAction(req, user) {
		Respond(r, &APIResponse{Code: ERROR, Message: "Unauthorized"})
		return
	}
	instanceKey, err := this.getInstanceKey(params["host"], params["port"])
	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: err.Error()})
		return
	}

	replicas, err, _ := inst.RepointReplicas(&instanceKey, req.URL.Query().Get("pattern"))
	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: err.Error()})
		return
	}

	Respond(r, &APIResponse{Code: OK, Message: fmt.Sprintf("Repointed %d replicas of %+v", len(replicas), instanceKey), Details: replicas})
}

// MakeCoPrimary attempts to make an instance co-primary with its own primary
func (this *HttpAPI) MakeCoPrimary(params martini.Params, r render.Render, req *http.Request, user auth.User) {
	if !isAuthorizedForAction(req, user) {
		Respond(r, &APIResponse{Code: ERROR, Message: "Unauthorized"})
		return
	}
	instanceKey, err := this.getInstanceKey(params["host"], params["port"])

	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: err.Error()})
		return
	}
	instance, err := inst.MakeCoPrimary(&instanceKey)
	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: err.Error()})
		return
	}

	Respond(r, &APIResponse{Code: OK, Message: fmt.Sprintf("Instance made co-primary: %+v", instance.Key), Details: instance})
}

// ResetReplication makes a replica forget about its primary, effectively breaking the replication
func (this *HttpAPI) ResetReplication(params martini.Params, r render.Render, req *http.Request, user auth.User) {
	if !isAuthorizedForAction(req, user) {
		Respond(r, &APIResponse{Code: ERROR, Message: "Unauthorized"})
		return
	}
	instanceKey, err := this.getInstanceKey(params["host"], params["port"])

	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: err.Error()})
		return
	}
	instance, err := inst.ResetReplicationOperation(&instanceKey)
	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: err.Error()})
		return
	}

	Respond(r, &APIResponse{Code: OK, Message: fmt.Sprintf("Replica reset on %+v", instance.Key), Details: instance})
}

// DetachReplicaPrimaryHost detaches a replica from its primary by setting an invalid
// (yet revertible) host name
func (this *HttpAPI) DetachReplicaPrimaryHost(params martini.Params, r render.Render, req *http.Request, user auth.User) {
	if !isAuthorizedForAction(req, user) {
		Respond(r, &APIResponse{Code: ERROR, Message: "Unauthorized"})
		return
	}
	instanceKey, err := this.getInstanceKey(params["host"], params["port"])

	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: err.Error()})
		return
	}
	instance, err := inst.DetachReplicaPrimaryHost(&instanceKey)
	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: err.Error()})
		return
	}

	Respond(r, &APIResponse{Code: OK, Message: fmt.Sprintf("Replica detached: %+v", instance.Key), Details: instance})
}

// ReattachReplicaPrimaryHost reverts a detachReplicaPrimaryHost command
// by resetting the original primary hostname in CHANGE MASTER TO
func (this *HttpAPI) ReattachReplicaPrimaryHost(params martini.Params, r render.Render, req *http.Request, user auth.User) {
	if !isAuthorizedForAction(req, user) {
		Respond(r, &APIResponse{Code: ERROR, Message: "Unauthorized"})
		return
	}
	instanceKey, err := this.getInstanceKey(params["host"], params["port"])

	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: err.Error()})
		return
	}
	instance, err := inst.ReattachReplicaPrimaryHost(&instanceKey)
	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: err.Error()})
		return
	}

	Respond(r, &APIResponse{Code: OK, Message: fmt.Sprintf("Replica reattached: %+v", instance.Key), Details: instance})
}

// EnableGTID attempts to enable GTID on a replica
func (this *HttpAPI) EnableGTID(params martini.Params, r render.Render, req *http.Request, user auth.User) {
	if !isAuthorizedForAction(req, user) {
		Respond(r, &APIResponse{Code: ERROR, Message: "Unauthorized"})
		return
	}
	instanceKey, err := this.getInstanceKey(params["host"], params["port"])

	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: err.Error()})
		return
	}
	instance, err := inst.EnableGTID(&instanceKey)
	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: err.Error()})
		return
	}

	Respond(r, &APIResponse{Code: OK, Message: fmt.Sprintf("Enabled GTID on %+v", instance.Key), Details: instance})
}

// DisableGTID attempts to disable GTID on a replica, and revert to binlog file:pos
func (this *HttpAPI) DisableGTID(params martini.Params, r render.Render, req *http.Request, user auth.User) {
	if !isAuthorizedForAction(req, user) {
		Respond(r, &APIResponse{Code: ERROR, Message: "Unauthorized"})
		return
	}
	instanceKey, err := this.getInstanceKey(params["host"], params["port"])

	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: err.Error()})
		return
	}
	instance, err := inst.DisableGTID(&instanceKey)
	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: err.Error()})
		return
	}

	Respond(r, &APIResponse{Code: OK, Message: fmt.Sprintf("Disabled GTID on %+v", instance.Key), Details: instance})
}

// LocateErrantGTID identifies the binlog positions for errant GTIDs on an instance
func (this *HttpAPI) LocateErrantGTID(params martini.Params, r render.Render, req *http.Request, user auth.User) {
	instanceKey, err := this.getInstanceKey(params["host"], params["port"])

	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: err.Error()})
		return
	}
	errantBinlogs, err := inst.LocateErrantGTID(&instanceKey)
	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: err.Error()})
		return
	}
	Respond(r, &APIResponse{Code: OK, Message: "located errant GTID", Details: errantBinlogs})
}

// ErrantGTIDResetPrimary removes errant transactions on a server by way of RESET MASTER
func (this *HttpAPI) ErrantGTIDResetPrimary(params martini.Params, r render.Render, req *http.Request, user auth.User) {
	if !isAuthorizedForAction(req, user) {
		Respond(r, &APIResponse{Code: ERROR, Message: "Unauthorized"})
		return
	}
	instanceKey, err := this.getInstanceKey(params["host"], params["port"])

	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: err.Error()})
		return
	}
	instance, err := inst.ErrantGTIDResetPrimary(&instanceKey)
	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: err.Error()})
		return
	}

	Respond(r, &APIResponse{Code: OK, Message: fmt.Sprintf("Removed errant GTID on %+v and issued a RESET MASTER", instance.Key), Details: instance})
}

// ErrantGTIDInjectEmpty removes errant transactions by injecting and empty transaction on the cluster's primary
func (this *HttpAPI) ErrantGTIDInjectEmpty(params martini.Params, r render.Render, req *http.Request, user auth.User) {
	if !isAuthorizedForAction(req, user) {
		Respond(r, &APIResponse{Code: ERROR, Message: "Unauthorized"})
		return
	}
	instanceKey, err := this.getInstanceKey(params["host"], params["port"])

	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: err.Error()})
		return
	}
	instance, clusterPrimary, countInjectedTransactions, err := inst.ErrantGTIDInjectEmpty(&instanceKey)
	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: err.Error()})
		return
	}

	Respond(r, &APIResponse{Code: OK, Message: fmt.Sprintf("Have injected %+v transactions on cluster primary %+v", countInjectedTransactions, clusterPrimary.Key), Details: instance})
}

// MoveBelow attempts to move an instance below its supposed sibling
func (this *HttpAPI) MoveBelow(params martini.Params, r render.Render, req *http.Request, user auth.User) {
	if !isAuthorizedForAction(req, user) {
		Respond(r, &APIResponse{Code: ERROR, Message: "Unauthorized"})
		return
	}
	instanceKey, err := this.getInstanceKey(params["host"], params["port"])
	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: err.Error()})
		return
	}
	siblingKey, err := this.getInstanceKey(params["siblingHost"], params["siblingPort"])
	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: err.Error()})
		return
	}

	instance, err := inst.MoveBelow(&instanceKey, &siblingKey)
	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: err.Error()})
		return
	}

	Respond(r, &APIResponse{Code: OK, Message: fmt.Sprintf("Instance %+v moved below %+v", instanceKey, siblingKey), Details: instance})
}

// MoveBelowGTID attempts to move an instance below another, via GTID
func (this *HttpAPI) MoveBelowGTID(params martini.Params, r render.Render, req *http.Request, user auth.User) {
	if !isAuthorizedForAction(req, user) {
		Respond(r, &APIResponse{Code: ERROR, Message: "Unauthorized"})
		return
	}
	instanceKey, err := this.getInstanceKey(params["host"], params["port"])
	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: err.Error()})
		return
	}
	belowKey, err := this.getInstanceKey(params["belowHost"], params["belowPort"])
	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: err.Error()})
		return
	}

	instance, err := inst.MoveBelowGTID(&instanceKey, &belowKey)
	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: err.Error()})
		return
	}

	Respond(r, &APIResponse{Code: OK, Message: fmt.Sprintf("Instance %+v moved below %+v via GTID", instanceKey, belowKey), Details: instance})
}

// MoveReplicasGTID attempts to move an instance below another, via GTID
func (this *HttpAPI) MoveReplicasGTID(params martini.Params, r render.Render, req *http.Request, user auth.User) {
	if !isAuthorizedForAction(req, user) {
		Respond(r, &APIResponse{Code: ERROR, Message: "Unauthorized"})
		return
	}
	instanceKey, err := this.getInstanceKey(params["host"], params["port"])
	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: err.Error()})
		return
	}
	belowKey, err := this.getInstanceKey(params["belowHost"], params["belowPort"])
	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: err.Error()})
		return
	}

	movedReplicas, _, err, errs := inst.MoveReplicasGTID(&instanceKey, &belowKey, req.URL.Query().Get("pattern"))
	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: err.Error()})
		return
	}

	Respond(r, &APIResponse{Code: OK, Message: fmt.Sprintf("Moved %d replicas of %+v below %+v via GTID; %d errors: %+v", len(movedReplicas), instanceKey, belowKey, len(errs), errs), Details: belowKey})
}

// TakeSiblings
func (this *HttpAPI) TakeSiblings(params martini.Params, r render.Render, req *http.Request, user auth.User) {
	if !isAuthorizedForAction(req, user) {
		Respond(r, &APIResponse{Code: ERROR, Message: "Unauthorized"})
		return
	}
	instanceKey, err := this.getInstanceKey(params["host"], params["port"])
	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: err.Error()})
		return
	}

	instance, count, err := inst.TakeSiblings(&instanceKey)
	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: err.Error()})
		return
	}

	Respond(r, &APIResponse{Code: OK, Message: fmt.Sprintf("Took %d siblings of %+v", count, instanceKey), Details: instance})
}

// TakePrimary
func (this *HttpAPI) TakePrimary(params martini.Params, r render.Render, req *http.Request, user auth.User) {
	if !isAuthorizedForAction(req, user) {
		Respond(r, &APIResponse{Code: ERROR, Message: "Unauthorized"})
		return
	}
	instanceKey, err := this.getInstanceKey(params["host"], params["port"])
	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: err.Error()})
		return
	}

	instance, err := inst.TakePrimary(&instanceKey, false)
	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: err.Error()})
		return
	}

	Respond(r, &APIResponse{Code: OK, Message: fmt.Sprintf("%+v took its primary", instanceKey), Details: instance})
}

// RelocateBelow attempts to move an instance below another, orchestrator choosing the best (potentially multi-step)
// relocation method
func (this *HttpAPI) RelocateBelow(params martini.Params, r render.Render, req *http.Request, user auth.User) {
	if !isAuthorizedForAction(req, user) {
		Respond(r, &APIResponse{Code: ERROR, Message: "Unauthorized"})
		return
	}
	instanceKey, err := this.getInstanceKey(params["host"], params["port"])
	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: err.Error()})
		return
	}
	belowKey, err := this.getInstanceKey(params["belowHost"], params["belowPort"])
	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: err.Error()})
		return
	}

	instance, err := inst.RelocateBelow(&instanceKey, &belowKey)
	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: err.Error()})
		return
	}

	Respond(r, &APIResponse{Code: OK, Message: fmt.Sprintf("Instance %+v relocated below %+v", instanceKey, belowKey), Details: instance})
}

// Relocates attempts to smartly relocate replicas of a given instance below another
func (this *HttpAPI) RelocateReplicas(params martini.Params, r render.Render, req *http.Request, user auth.User) {
	if !isAuthorizedForAction(req, user) {
		Respond(r, &APIResponse{Code: ERROR, Message: "Unauthorized"})
		return
	}
	instanceKey, err := this.getInstanceKey(params["host"], params["port"])
	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: err.Error()})
		return
	}
	belowKey, err := this.getInstanceKey(params["belowHost"], params["belowPort"])
	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: err.Error()})
		return
	}

	replicas, _, err, errs := inst.RelocateReplicas(&instanceKey, &belowKey, req.URL.Query().Get("pattern"))
	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: err.Error()})
		return
	}

	Respond(r, &APIResponse{Code: OK, Message: fmt.Sprintf("Relocated %d replicas of %+v below %+v; %d errors: %+v", len(replicas), instanceKey, belowKey, len(errs), errs), Details: replicas})
}

// RegroupReplicas attempts to pick a replica of a given instance and make it take its siblings, using any
// method possible (GTID, binlog servers)
func (this *HttpAPI) RegroupReplicas(params martini.Params, r render.Render, req *http.Request, user auth.User) {
	if !isAuthorizedForAction(req, user) {
		Respond(r, &APIResponse{Code: ERROR, Message: "Unauthorized"})
		return
	}
	instanceKey, err := this.getInstanceKey(params["host"], params["port"])
	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: err.Error()})
		return
	}

	lostReplicas, equalReplicas, aheadReplicas, cannotReplicateReplicas, promotedReplica, err := inst.RegroupReplicas(&instanceKey, false, nil, nil)
	lostReplicas = append(lostReplicas, cannotReplicateReplicas...)
	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: err.Error()})
		return
	}

	Respond(r, &APIResponse{Code: OK, Message: fmt.Sprintf("promoted replica: %s, lost: %d, trivial: %d, pseudo-gtid: %d",
		promotedReplica.Key.DisplayString(), len(lostReplicas), len(equalReplicas), len(aheadReplicas)), Details: promotedReplica.Key})
}

// RegroupReplicasGTID attempts to pick a replica of a given instance and make it take its siblings, efficiently, using GTID
func (this *HttpAPI) RegroupReplicasGTID(params martini.Params, r render.Render, req *http.Request, user auth.User) {
	if !isAuthorizedForAction(req, user) {
		Respond(r, &APIResponse{Code: ERROR, Message: "Unauthorized"})
		return
	}
	instanceKey, err := this.getInstanceKey(params["host"], params["port"])
	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: err.Error()})
		return
	}

	lostReplicas, movedReplicas, cannotReplicateReplicas, promotedReplica, err := inst.RegroupReplicasGTID(&instanceKey, false, nil, nil, nil)
	lostReplicas = append(lostReplicas, cannotReplicateReplicas...)

	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: err.Error()})
		return
	}

	Respond(r, &APIResponse{Code: OK, Message: fmt.Sprintf("promoted replica: %s, lost: %d, moved: %d",
		promotedReplica.Key.DisplayString(), len(lostReplicas), len(movedReplicas)), Details: promotedReplica.Key})
}

// RegroupReplicasBinlogServers attempts to pick a replica of a given instance and make it take its siblings, efficiently, using GTID
func (this *HttpAPI) RegroupReplicasBinlogServers(params martini.Params, r render.Render, req *http.Request, user auth.User) {
	if !isAuthorizedForAction(req, user) {
		Respond(r, &APIResponse{Code: ERROR, Message: "Unauthorized"})
		return
	}
	instanceKey, err := this.getInstanceKey(params["host"], params["port"])
	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: err.Error()})
		return
	}

	_, promotedBinlogServer, err := inst.RegroupReplicasBinlogServers(&instanceKey, false)

	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: err.Error()})
		return
	}

	Respond(r, &APIResponse{Code: OK, Message: fmt.Sprintf("promoted binlog server: %s",
		promotedBinlogServer.Key.DisplayString()), Details: promotedBinlogServer.Key})
}

// SkipQuery skips a single query on a failed replication instance
func (this *HttpAPI) SkipQuery(params martini.Params, r render.Render, req *http.Request, user auth.User) {
	if !isAuthorizedForAction(req, user) {
		Respond(r, &APIResponse{Code: ERROR, Message: "Unauthorized"})
		return
	}
	instanceKey, err := this.getInstanceKey(params["host"], params["port"])

	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: err.Error()})
		return
	}
	instance, err := inst.SkipQuery(&instanceKey)
	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: err.Error()})
		return
	}

	Respond(r, &APIResponse{Code: OK, Message: fmt.Sprintf("Query skipped on %+v", instance.Key), Details: instance})
}

// StartReplication starts replication on given instance
func (this *HttpAPI) StartReplication(params martini.Params, r render.Render, req *http.Request, user auth.User) {
	if !isAuthorizedForAction(req, user) {
		Respond(r, &APIResponse{Code: ERROR, Message: "Unauthorized"})
		return
	}
	instanceKey, err := this.getInstanceKey(params["host"], params["port"])

	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: err.Error()})
		return
	}
	instance, err := inst.StartReplication(&instanceKey)
	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: err.Error()})
		return
	}

	Respond(r, &APIResponse{Code: OK, Message: fmt.Sprintf("Replica started: %+v", instance.Key), Details: instance})
}

// RestartReplication stops & starts replication on given instance
func (this *HttpAPI) RestartReplication(params martini.Params, r render.Render, req *http.Request, user auth.User) {
	if !isAuthorizedForAction(req, user) {
		Respond(r, &APIResponse{Code: ERROR, Message: "Unauthorized"})
		return
	}
	instanceKey, err := this.getInstanceKey(params["host"], params["port"])

	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: err.Error()})
		return
	}
	instance, err := inst.RestartReplication(&instanceKey)
	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: err.Error()})
		return
	}

	Respond(r, &APIResponse{Code: OK, Message: fmt.Sprintf("Replica restarted: %+v", instance.Key), Details: instance})
}

// StopReplication stops replication on given instance
func (this *HttpAPI) StopReplication(params martini.Params, r render.Render, req *http.Request, user auth.User) {
	if !isAuthorizedForAction(req, user) {
		Respond(r, &APIResponse{Code: ERROR, Message: "Unauthorized"})
		return
	}
	instanceKey, err := this.getInstanceKey(params["host"], params["port"])

	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: err.Error()})
		return
	}
	instance, err := inst.StopReplication(&instanceKey)
	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: err.Error()})
		return
	}

	Respond(r, &APIResponse{Code: OK, Message: fmt.Sprintf("Replica stopped: %+v", instance.Key), Details: instance})
}

// StopReplicationNicely stops replication on given instance, such that sql thead is aligned with IO thread
func (this *HttpAPI) StopReplicationNicely(params martini.Params, r render.Render, req *http.Request, user auth.User) {
	if !isAuthorizedForAction(req, user) {
		Respond(r, &APIResponse{Code: ERROR, Message: "Unauthorized"})
		return
	}
	instanceKey, err := this.getInstanceKey(params["host"], params["port"])

	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: err.Error()})
		return
	}
	instance, err := inst.StopReplicationNicely(&instanceKey, 0)
	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: err.Error()})
		return
	}

	Respond(r, &APIResponse{Code: OK, Message: fmt.Sprintf("Replica stopped nicely: %+v", instance.Key), Details: instance})
}

// FlushBinaryLogs runs a single FLUSH BINARY LOGS
func (this *HttpAPI) FlushBinaryLogs(params martini.Params, r render.Render, req *http.Request, user auth.User) {
	if !isAuthorizedForAction(req, user) {
		Respond(r, &APIResponse{Code: ERROR, Message: "Unauthorized"})
		return
	}
	instanceKey, err := this.getInstanceKey(params["host"], params["port"])

	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: err.Error()})
		return
	}
	instance, err := inst.FlushBinaryLogs(&instanceKey, 1)
	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: err.Error()})
		return
	}

	Respond(r, &APIResponse{Code: OK, Message: fmt.Sprintf("Binary logs flushed on: %+v", instance.Key), Details: instance})
}

// PurgeBinaryLogs purges binary logs up to given binlog file
func (this *HttpAPI) PurgeBinaryLogs(params martini.Params, r render.Render, req *http.Request, user auth.User) {
	if !isAuthorizedForAction(req, user) {
		Respond(r, &APIResponse{Code: ERROR, Message: "Unauthorized"})
		return
	}
	instanceKey, err := this.getInstanceKey(params["host"], params["port"])

	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: err.Error()})
		return
	}
	logFile := params["logFile"]
	if logFile == "" {
		Respond(r, &APIResponse{Code: ERROR, Message: "purge-binary-logs: expected log file name or 'latest'"})
		return
	}
	force := (req.URL.Query().Get("force") == "true") || (params["force"] == "true")
	var instance *inst.Instance
	if logFile == "latest" {
		instance, err = inst.PurgeBinaryLogsToLatest(&instanceKey, force)
	} else {
		instance, err = inst.PurgeBinaryLogsTo(&instanceKey, logFile, force)
	}
	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: err.Error()})
		return
	}

	Respond(r, &APIResponse{Code: OK, Message: fmt.Sprintf("Binary logs flushed on: %+v", instance.Key), Details: instance})
}

// RestartReplicationStatements receives a query to execute that requires a replication restart to apply.
// As an example, this may be `set global rpl_semi_sync_slave_enabled=1`. orchestrator will check
// replication status on given host and will wrap with appropriate stop/start statements, if need be.
func (this *HttpAPI) RestartReplicationStatements(params martini.Params, r render.Render, req *http.Request, user auth.User) {
	if !isAuthorizedForAction(req, user) {
		Respond(r, &APIResponse{Code: ERROR, Message: "Unauthorized"})
		return
	}
	instanceKey, err := this.getInstanceKey(params["host"], params["port"])

	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: err.Error()})
		return
	}

	query := req.URL.Query().Get("q")
	statements, err := inst.GetReplicationRestartPreserveStatements(&instanceKey, query)

	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: err.Error()})
		return
	}

	Respond(r, &APIResponse{Code: OK, Message: fmt.Sprintf("statements for: %+v", instanceKey), Details: statements})
}

// CanReplicateFrom attempts to move an instance below another via pseudo GTID matching of binlog entries
func (this *HttpAPI) CanReplicateFrom(params martini.Params, r render.Render, req *http.Request, user auth.User) {
	instanceKey, err := this.getInstanceKey(params["host"], params["port"])
	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: err.Error()})
		return
	}
	instance, found, err := inst.ReadInstance(&instanceKey)
	if (!found) || (err != nil) {
		Respond(r, &APIResponse{Code: ERROR, Message: fmt.Sprintf("Cannot read instance: %+v", instanceKey)})
		return
	}
	belowKey, err := this.getInstanceKey(params["belowHost"], params["belowPort"])
	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: err.Error()})
		return
	}
	belowInstance, found, err := inst.ReadInstance(&belowKey)
	if (!found) || (err != nil) {
		Respond(r, &APIResponse{Code: ERROR, Message: fmt.Sprintf("Cannot read instance: %+v", belowKey)})
		return
	}

	canReplicate, err := instance.CanReplicateFrom(belowInstance)
	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: err.Error()})
		return
	}

	Respond(r, &APIResponse{Code: OK, Message: fmt.Sprintf("%t", canReplicate), Details: belowKey})
}

// CanReplicateFromGTID attempts to move an instance below another via GTID.
func (this *HttpAPI) CanReplicateFromGTID(params martini.Params, r render.Render, req *http.Request, user auth.User) {
	instanceKey, err := this.getInstanceKey(params["host"], params["port"])
	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: err.Error()})
		return
	}
	instance, found, err := inst.ReadInstance(&instanceKey)
	if (!found) || (err != nil) {
		Respond(r, &APIResponse{Code: ERROR, Message: fmt.Sprintf("Cannot read instance: %+v", instanceKey)})
		return
	}
	belowKey, err := this.getInstanceKey(params["belowHost"], params["belowPort"])
	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: err.Error()})
		return
	}
	belowInstance, found, err := inst.ReadInstance(&belowKey)
	if (!found) || (err != nil) {
		Respond(r, &APIResponse{Code: ERROR, Message: fmt.Sprintf("Cannot read instance: %+v", belowKey)})
		return
	}

	canReplicate, err := instance.CanReplicateFrom(belowInstance)
	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: err.Error()})
		return
	}
	if !canReplicate {
		Respond(r, &APIResponse{Code: OK, Message: fmt.Sprintf("%t", canReplicate), Details: belowKey})
		return
	}
	err = inst.CheckMoveViaGTID(instance, belowInstance)
	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: err.Error()})
		return
	}
	canReplicate = (err == nil)

	Respond(r, &APIResponse{Code: OK, Message: fmt.Sprintf("%t", canReplicate), Details: belowKey})
}

// SetReadOnly sets the global read_only variable
func (this *HttpAPI) SetReadOnly(params martini.Params, r render.Render, req *http.Request, user auth.User) {
	if !isAuthorizedForAction(req, user) {
		Respond(r, &APIResponse{Code: ERROR, Message: "Unauthorized"})
		return
	}
	instanceKey, err := this.getInstanceKey(params["host"], params["port"])

	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: err.Error()})
		return
	}
	instance, err := inst.SetReadOnly(&instanceKey, true)
	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: err.Error()})
		return
	}

	Respond(r, &APIResponse{Code: OK, Message: "Server set as read-only", Details: instance})
}

// SetWriteable clear the global read_only variable
func (this *HttpAPI) SetWriteable(params martini.Params, r render.Render, req *http.Request, user auth.User) {
	if !isAuthorizedForAction(req, user) {
		Respond(r, &APIResponse{Code: ERROR, Message: "Unauthorized"})
		return
	}
	instanceKey, err := this.getInstanceKey(params["host"], params["port"])

	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: err.Error()})
		return
	}
	instance, err := inst.SetReadOnly(&instanceKey, false)
	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: err.Error()})
		return
	}

	Respond(r, &APIResponse{Code: OK, Message: "Server set as writeable", Details: instance})
}

// KillQuery kills a query running on a server
func (this *HttpAPI) KillQuery(params martini.Params, r render.Render, req *http.Request, user auth.User) {
	if !isAuthorizedForAction(req, user) {
		Respond(r, &APIResponse{Code: ERROR, Message: "Unauthorized"})
		return
	}
	instanceKey, _ := this.getInstanceKey(params["host"], params["port"])
	processId, err := strconv.ParseInt(params["process"], 10, 0)
	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: err.Error()})
		return
	}

	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: err.Error()})
		return
	}
	instance, err := inst.KillQuery(&instanceKey, processId)
	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: err.Error()})
		return
	}

	Respond(r, &APIResponse{Code: OK, Message: fmt.Sprintf("Query killed on : %+v", instance.Key), Details: instance})
}

// AsciiTopology returns an ascii graph of cluster's instances
func (this *HttpAPI) asciiTopology(params martini.Params, r render.Render, req *http.Request, tabulated bool, printTags bool) {
	clusterName, err := figureClusterName(getClusterHint(params))
	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: fmt.Sprintf("%+v", err)})
		return
	}

	asciiOutput, err := inst.ASCIITopology(clusterName, "", tabulated, printTags)
	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: fmt.Sprintf("%+v", err)})
		return
	}

	Respond(r, &APIResponse{Code: OK, Message: fmt.Sprintf("Topology for cluster %s", clusterName), Details: asciiOutput})
}

// SnapshotTopologies triggers orchestrator to record a snapshot of host/primary for all known hosts.
func (this *HttpAPI) SnapshotTopologies(params martini.Params, r render.Render, req *http.Request) {
	start := time.Now()
	if err := inst.SnapshotTopologies(); err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: fmt.Sprintf("%+v", err), Details: fmt.Sprintf("Took %v", time.Since(start))})
		return
	}

	Respond(r, &APIResponse{Code: OK, Message: "Topology Snapshot completed", Details: fmt.Sprintf("Took %v", time.Since(start))})
}

// AsciiTopology returns an ascii graph of cluster's instances
func (this *HttpAPI) AsciiTopology(params martini.Params, r render.Render, req *http.Request) {
	this.asciiTopology(params, r, req, false, false)
}

// AsciiTopology returns an ascii graph of cluster's instances
func (this *HttpAPI) AsciiTopologyTabulated(params martini.Params, r render.Render, req *http.Request) {
	this.asciiTopology(params, r, req, true, false)
}

// AsciiTopologyTags returns an ascii graph of cluster's instances and instance tags
func (this *HttpAPI) AsciiTopologyTags(params martini.Params, r render.Render, req *http.Request) {
	this.asciiTopology(params, r, req, false, true)
}

// Cluster provides list of instances in given cluster
func (this *HttpAPI) Cluster(params martini.Params, r render.Render, req *http.Request) {
	clusterName, err := figureClusterName(getClusterHint(params))
	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: fmt.Sprintf("%+v", err)})
		return
	}

	instances, err := inst.ReadClusterInstances(clusterName)

	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: fmt.Sprintf("%+v", err)})
		return
	}

	r.JSON(http.StatusOK, instances)
}

// ClusterByAlias provides list of instances in given cluster
func (this *HttpAPI) ClusterByAlias(params martini.Params, r render.Render, req *http.Request) {
	clusterName, err := inst.GetClusterByAlias(params["clusterAlias"])
	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: fmt.Sprintf("%+v", err)})
		return
	}

	params["clusterName"] = clusterName
	this.Cluster(params, r, req)
}

// ClusterByInstance provides list of instances in cluster an instance belongs to
func (this *HttpAPI) ClusterByInstance(params martini.Params, r render.Render, req *http.Request) {
	instanceKey, err := this.getInstanceKey(params["host"], params["port"])
	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: err.Error()})
		return
	}
	instance, found, err := inst.ReadInstance(&instanceKey)
	if (!found) || (err != nil) {
		Respond(r, &APIResponse{Code: ERROR, Message: fmt.Sprintf("Cannot read instance: %+v", instanceKey)})
		return
	}

	params["clusterName"] = instance.ClusterName
	this.Cluster(params, r, req)
}

// ClusterInfo provides details of a given cluster
func (this *HttpAPI) ClusterInfo(params martini.Params, r render.Render, req *http.Request) {
	clusterName, err := figureClusterName(getClusterHint(params))
	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: fmt.Sprintf("%+v", err)})
		return
	}
	clusterInfo, err := inst.ReadClusterInfo(clusterName)

	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: fmt.Sprintf("%+v", err)})
		return
	}

	r.JSON(http.StatusOK, clusterInfo)
}

// Cluster provides list of instances in given cluster
func (this *HttpAPI) ClusterInfoByAlias(params martini.Params, r render.Render, req *http.Request) {
	clusterName, err := inst.GetClusterByAlias(params["clusterAlias"])
	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: fmt.Sprintf("%+v", err)})
		return
	}

	params["clusterName"] = clusterName
	this.ClusterInfo(params, r, req)
}

// ClusterOSCReplicas returns heuristic list of OSC replicas
func (this *HttpAPI) ClusterOSCReplicas(params martini.Params, r render.Render, req *http.Request) {
	clusterName, err := figureClusterName(getClusterHint(params))
	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: fmt.Sprintf("%+v", err)})
		return
	}

	instances, err := inst.GetClusterOSCReplicas(clusterName)
	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: fmt.Sprintf("%+v", err)})
		return
	}

	r.JSON(http.StatusOK, instances)
}

// SetClusterAlias will change an alias for a given clustername
func (this *HttpAPI) SetClusterAliasManualOverride(params martini.Params, r render.Render, req *http.Request, user auth.User) {
	if !isAuthorizedForAction(req, user) {
		Respond(r, &APIResponse{Code: ERROR, Message: "Unauthorized"})
		return
	}
	clusterName := params["clusterName"]
	alias := req.URL.Query().Get("alias")

	var err error
	if orcraft.IsRaftEnabled() {
		_, err = orcraft.PublishCommand("set-cluster-alias-manual-override", []string{clusterName, alias})
	} else {
		err = inst.SetClusterAliasManualOverride(clusterName, alias)
	}

	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: fmt.Sprintf("%+v", err)})
		return
	}
	Respond(r, &APIResponse{Code: OK, Message: fmt.Sprintf("Cluster %s now has alias '%s'", clusterName, alias)})
}

// Clusters provides list of known clusters
func (this *HttpAPI) Clusters(params martini.Params, r render.Render, req *http.Request) {
	clusterNames, err := inst.ReadClusters()

	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: fmt.Sprintf("%+v", err)})
		return
	}

	r.JSON(http.StatusOK, clusterNames)
}

// ClustersInfo provides list of known clusters, along with some added metadata per cluster
func (this *HttpAPI) ClustersInfo(params martini.Params, r render.Render, req *http.Request) {
	clustersInfo, err := inst.ReadClustersInfo("")

	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: fmt.Sprintf("%+v", err)})
		return
	}

	r.JSON(http.StatusOK, clustersInfo)
}

// Tags lists existing tags for a given instance
func (this *HttpAPI) Tags(params martini.Params, r render.Render, req *http.Request) {
	instanceKey, err := this.getInstanceKey(params["host"], params["port"])
	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: err.Error()})
		return
	}

	tags, err := inst.ReadInstanceTags(&instanceKey)
	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: err.Error()})
		return
	}
	tagStrings := []string{}
	for _, tag := range tags {
		tagStrings = append(tagStrings, tag.String())
	}
	r.JSON(http.StatusOK, tagStrings)
}

// TagValue returns a given tag's value for a specific instance
func (this *HttpAPI) TagValue(params martini.Params, r render.Render, req *http.Request) {
	instanceKey, err := this.getInstanceKey(params["host"], params["port"])
	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: err.Error()})
		return
	}
	tag, err := getTag(params, req)
	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: err.Error()})
		return
	}
	tagExists, err := inst.ReadInstanceTag(&instanceKey, tag)
	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: err.Error()})
		return
	}
	if tagExists {
		r.JSON(http.StatusOK, tag.TagValue)
	} else {
		Respond(r, &APIResponse{Code: ERROR, Message: fmt.Sprintf("tag %s not found for %+v", tag.TagName, instanceKey)})
	}
}

// Tagged return instance keys tagged by "tag" query param
func (this *HttpAPI) Tagged(params martini.Params, r render.Render, req *http.Request) {
	tagsString := req.URL.Query().Get("tag")
	instanceKeyMap, err := inst.GetInstanceKeysByTags(tagsString)
	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: err.Error()})
		return
	}

	r.JSON(http.StatusOK, instanceKeyMap.GetInstanceKeys())
}

// Tags adds a tag to a given instance
func (this *HttpAPI) Tag(params martini.Params, r render.Render, req *http.Request) {
	instanceKey, err := this.getInstanceKey(params["host"], params["port"])
	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: err.Error()})
		return
	}

	tag, err := getTag(params, req)
	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: err.Error()})
		return
	}
	if orcraft.IsRaftEnabled() {
		_, err = orcraft.PublishCommand("put-instance-tag", inst.InstanceTag{Key: instanceKey, T: *tag})
	} else {
		err = inst.PutInstanceTag(&instanceKey, tag)
	}
	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: err.Error()})
		return
	}

	Respond(r, &APIResponse{Code: OK, Message: fmt.Sprintf("%+v tagged with %s", instanceKey, tag.String()), Details: instanceKey})
}

// Untag removes a tag from an instance
func (this *HttpAPI) Untag(params martini.Params, r render.Render, req *http.Request) {
	instanceKey, err := this.getInstanceKey(params["host"], params["port"])
	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: err.Error()})
		return
	}

	tag, err := getTag(params, req)
	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: err.Error()})
		return
	}
	untagged, err := inst.Untag(&instanceKey, tag)
	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: err.Error()})
		return
	}

	Respond(r, &APIResponse{Code: OK, Message: fmt.Sprintf("%s removed from %+v instances", tag.TagName, len(*untagged)), Details: untagged.GetInstanceKeys()})
}

// UntagAll removes a tag from all matching instances
func (this *HttpAPI) UntagAll(params martini.Params, r render.Render, req *http.Request) {
	tag, err := getTag(params, req)
	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: err.Error()})
		return
	}
	untagged, err := inst.Untag(nil, tag)
	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: err.Error()})
		return
	}

	Respond(r, &APIResponse{Code: OK, Message: fmt.Sprintf("%s removed from %+v instances", tag.TagName, len(*untagged)), Details: untagged.GetInstanceKeys()})
}

// SubmitPrimariesToKvStores writes a cluster's primary (or all clusters primaries) to kv stores.
// This should generally only happen once in a lifetime of a cluster. Otherwise KV
// stores are updated via failovers.
func (this *HttpAPI) SubmitPrimariesToKvStores(params martini.Params, r render.Render, req *http.Request) {
	clusterName, err := getClusterNameIfExists(params)
	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: fmt.Sprintf("%+v", err)})
		return
	}
	kvPairs, submittedCount, err := logic.SubmitPrimariesToKvStores(clusterName, true)
	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: fmt.Sprintf("%+v", err)})
		return
	}
	Respond(r, &APIResponse{Code: OK, Message: fmt.Sprintf("Submitted %d primaries", submittedCount), Details: kvPairs})
}

// Clusters provides list of known primaries
func (this *HttpAPI) Primaries(params martini.Params, r render.Render, req *http.Request) {
	instances, err := inst.ReadWriteableClustersPrimaries()

	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: fmt.Sprintf("%+v", err)})
		return
	}

	r.JSON(http.StatusOK, instances)
}

// ClusterPrimary returns the writable primary of a given cluster
func (this *HttpAPI) ClusterPrimary(params martini.Params, r render.Render, req *http.Request) {
	clusterName, err := figureClusterName(getClusterHint(params))
	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: fmt.Sprintf("%+v", err)})
		return
	}

	primaries, err := inst.ReadClusterPrimary(clusterName)
	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: fmt.Sprintf("%+v", err)})
		return
	}
	if len(primaries) == 0 {
		Respond(r, &APIResponse{Code: ERROR, Message: fmt.Sprintf("No primaries found for %+v", clusterName)})
		return
	}

	r.JSON(http.StatusOK, primaries[0])
}

// Downtimed lists downtimed instances, potentially filtered by cluster
func (this *HttpAPI) Downtimed(params martini.Params, r render.Render, req *http.Request) {
	clusterName, err := getClusterNameIfExists(params)
	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: fmt.Sprintf("%+v", err)})
		return
	}

	instances, err := inst.ReadDowntimedInstances(clusterName)
	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: fmt.Sprintf("%+v", err)})
		return
	}

	r.JSON(http.StatusOK, instances)
}

// AllInstances lists all known instances
func (this *HttpAPI) AllInstances(params martini.Params, r render.Render, req *http.Request) {
	instances, err := inst.SearchInstances("")

	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: fmt.Sprintf("%+v", err)})
		return
	}

	r.JSON(http.StatusOK, instances)
}

// Search provides list of instances matching given search param via various criteria.
func (this *HttpAPI) Search(params martini.Params, r render.Render, req *http.Request) {
	searchString := params["searchString"]
	if searchString == "" {
		searchString = req.URL.Query().Get("s")
	}
	instances, err := inst.SearchInstances(searchString)

	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: fmt.Sprintf("%+v", err)})
		return
	}

	r.JSON(http.StatusOK, instances)
}

// Problems provides list of instances with known problems
func (this *HttpAPI) Problems(params martini.Params, r render.Render, req *http.Request) {
	clusterName := params["clusterName"]
	instances, err := inst.ReadProblemInstances(clusterName)

	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: fmt.Sprintf("%+v", err)})
		return
	}

	r.JSON(http.StatusOK, instances)
}

// Audit provides list of audit entries by given page number
func (this *HttpAPI) Audit(params martini.Params, r render.Render, req *http.Request) {
	page, err := strconv.Atoi(params["page"])
	if err != nil || page < 0 {
		page = 0
	}
	var auditedInstanceKey *inst.InstanceKey
	if instanceKey, err := this.getInstanceKey(params["host"], params["port"]); err == nil {
		auditedInstanceKey = &instanceKey
	}

	audits, err := inst.ReadRecentAudit(auditedInstanceKey, page)

	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: fmt.Sprintf("%+v", err)})
		return
	}

	r.JSON(http.StatusOK, audits)
}

// HostnameResolveCache shows content of in-memory hostname cache
func (this *HttpAPI) HostnameResolveCache(params martini.Params, r render.Render, req *http.Request) {
	content, err := inst.HostnameResolveCache()

	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: fmt.Sprintf("%+v", err)})
		return
	}

	Respond(r, &APIResponse{Code: OK, Message: "Cache retrieved", Details: content})
}

// ResetHostnameResolveCache clears in-memory hostname resovle cache
func (this *HttpAPI) ResetHostnameResolveCache(params martini.Params, r render.Render, req *http.Request, user auth.User) {
	if !isAuthorizedForAction(req, user) {
		Respond(r, &APIResponse{Code: ERROR, Message: "Unauthorized"})
		return
	}
	err := inst.ResetHostnameResolveCache()

	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: fmt.Sprintf("%+v", err)})
		return
	}

	Respond(r, &APIResponse{Code: OK, Message: "Hostname cache cleared"})
}

// DeregisterHostnameUnresolve deregisters the unresolve name used previously
func (this *HttpAPI) DeregisterHostnameUnresolve(params martini.Params, r render.Render, req *http.Request, user auth.User) {
	if !isAuthorizedForAction(req, user) {
		Respond(r, &APIResponse{Code: ERROR, Message: "Unauthorized"})
		return
	}

	var instanceKey *inst.InstanceKey
	if instKey, err := this.getInstanceKey(params["host"], params["port"]); err == nil {
		instanceKey = &instKey
	}

	var err error
	registration := inst.NewHostnameDeregistration(instanceKey)
	if orcraft.IsRaftEnabled() {
		_, err = orcraft.PublishCommand("register-hostname-unresolve", registration)
	} else {
		err = inst.RegisterHostnameUnresolve(registration)
	}
	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: fmt.Sprintf("%+v", err)})
		return
	}
	Respond(r, &APIResponse{Code: OK, Message: "Hostname deregister unresolve completed", Details: instanceKey})
}

// RegisterHostnameUnresolve registers the unresolve name to use
func (this *HttpAPI) RegisterHostnameUnresolve(params martini.Params, r render.Render, req *http.Request, user auth.User) {
	if !isAuthorizedForAction(req, user) {
		Respond(r, &APIResponse{Code: ERROR, Message: "Unauthorized"})
		return
	}

	var instanceKey *inst.InstanceKey
	if instKey, err := this.getInstanceKey(params["host"], params["port"]); err == nil {
		instanceKey = &instKey
	}

	hostname := params["virtualname"]
	var err error
	registration := inst.NewHostnameRegistration(instanceKey, hostname)
	if orcraft.IsRaftEnabled() {
		_, err = orcraft.PublishCommand("register-hostname-unresolve", registration)
	} else {
		err = inst.RegisterHostnameUnresolve(registration)
	}
	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: fmt.Sprintf("%+v", err)})
		return
	}
	Respond(r, &APIResponse{Code: OK, Message: "Hostname register unresolve completed", Details: instanceKey})
}

// SubmitPoolInstances (re-)applies the list of hostnames for a given pool
func (this *HttpAPI) SubmitPoolInstances(params martini.Params, r render.Render, req *http.Request, user auth.User) {
	if !isAuthorizedForAction(req, user) {
		Respond(r, &APIResponse{Code: ERROR, Message: "Unauthorized"})
		return
	}
	pool := params["pool"]
	instances := req.URL.Query().Get("instances")

	var err error
	submission := inst.NewPoolInstancesSubmission(pool, instances)
	if orcraft.IsRaftEnabled() {
		_, err = orcraft.PublishCommand("submit-pool-instances", submission)
	} else {
		err = inst.ApplyPoolInstances(submission)
	}
	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: fmt.Sprintf("%+v", err)})
		return
	}

	Respond(r, &APIResponse{Code: OK, Message: fmt.Sprintf("Applied %s pool instances", pool), Details: pool})
}

// SubmitPoolHostnames (re-)applies the list of hostnames for a given pool
func (this *HttpAPI) ReadClusterPoolInstancesMap(params martini.Params, r render.Render, req *http.Request, user auth.User) {
	if !isAuthorizedForAction(req, user) {
		Respond(r, &APIResponse{Code: ERROR, Message: "Unauthorized"})
		return
	}
	clusterName := params["clusterName"]
	pool := params["pool"]

	poolInstancesMap, err := inst.ReadClusterPoolInstancesMap(clusterName, pool)
	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: fmt.Sprintf("%+v", err)})
		return
	}

	Respond(r, &APIResponse{Code: OK, Message: fmt.Sprintf("Read pool instances for cluster %s", clusterName), Details: poolInstancesMap})
}

// GetHeuristicClusterPoolInstances returns instances belonging to a cluster's pool
func (this *HttpAPI) GetHeuristicClusterPoolInstances(params martini.Params, r render.Render, req *http.Request, user auth.User) {
	if !isAuthorizedForAction(req, user) {
		Respond(r, &APIResponse{Code: ERROR, Message: "Unauthorized"})
		return
	}
	clusterName, err := figureClusterName(getClusterHint(params))
	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: fmt.Sprintf("%+v", err)})
		return
	}
	pool := params["pool"]

	instances, err := inst.GetHeuristicClusterPoolInstances(clusterName, pool)
	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: fmt.Sprintf("%+v", err)})
		return
	}

	Respond(r, &APIResponse{Code: OK, Message: fmt.Sprintf("Heuristic pool instances for cluster %s", clusterName), Details: instances})
}

// GetHeuristicClusterPoolInstances returns instances belonging to a cluster's pool
func (this *HttpAPI) GetHeuristicClusterPoolInstancesLag(params martini.Params, r render.Render, req *http.Request, user auth.User) {
	if !isAuthorizedForAction(req, user) {
		Respond(r, &APIResponse{Code: ERROR, Message: "Unauthorized"})
		return
	}
	clusterName, err := inst.ReadClusterNameByAlias(params["clusterName"])
	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: fmt.Sprintf("%+v", err)})
		return
	}
	pool := params["pool"]

	lag, err := inst.GetHeuristicClusterPoolInstancesLag(clusterName, pool)
	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: fmt.Sprintf("%+v", err)})
		return
	}

	Respond(r, &APIResponse{Code: OK, Message: fmt.Sprintf("Heuristic pool lag for cluster %s", clusterName), Details: lag})
}

// ReloadClusterAlias clears in-memory hostname resovle cache
func (this *HttpAPI) ReloadClusterAlias(params martini.Params, r render.Render, req *http.Request, user auth.User) {
	if !isAuthorizedForAction(req, user) {
		Respond(r, &APIResponse{Code: ERROR, Message: "Unauthorized"})
		return
	}

	Respond(r, &APIResponse{Code: ERROR, Message: "This API call has been retired"})
}

// BulkPromotionRules returns a list of the known promotion rules for each instance
func (this *HttpAPI) BulkPromotionRules(params martini.Params, r render.Render, req *http.Request, user auth.User) {
	if !isAuthorizedForAction(req, user) {
		Respond(r, &APIResponse{Code: ERROR, Message: "Unauthorized"})
		return
	}

	promotionRules, err := inst.BulkReadCandidateDatabaseInstance()
	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: fmt.Sprintf("%+v", err)})
		return
	}

	r.JSON(http.StatusOK, promotionRules)
}

// BulkInstances returns a list of all known instances
func (this *HttpAPI) BulkInstances(params martini.Params, r render.Render, req *http.Request, user auth.User) {
	if !isAuthorizedForAction(req, user) {
		Respond(r, &APIResponse{Code: ERROR, Message: "Unauthorized"})
		return
	}

	instances, err := inst.BulkReadInstance()
	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: fmt.Sprintf("%+v", err)})
		return
	}

	r.JSON(http.StatusOK, instances)
}

// DiscoveryMetricsRaw will return the last X seconds worth of discovery information in time based order as a JSON array
func (this *HttpAPI) DiscoveryMetricsRaw(params martini.Params, r render.Render, req *http.Request, user auth.User) {
	seconds, err := strconv.Atoi(params["seconds"])
	if err != nil || seconds <= 0 {
		Respond(r, &APIResponse{Code: ERROR, Message: "Invalid value provided for seconds"})
		return
	}

	refTime := time.Now().Add(-time.Duration(seconds) * time.Second)
	json, err := discovery.JSONSince(discoveryMetrics, refTime)
	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: "Unable to determine start time. Perhaps seconds value is wrong?"})
		return
	}
	log.Debugf("DiscoveryMetricsRaw data: retrieved %d entries from discovery.MC", len(json))

	r.JSON(http.StatusOK, json)
}

// DiscoveryMetricsAggregated will return a single set of aggregated metrics for raw values collected since the
// specified time.
func (this *HttpAPI) DiscoveryMetricsAggregated(params martini.Params, r render.Render, req *http.Request, user auth.User) {
	seconds, _ := strconv.Atoi(params["seconds"])

	refTime := time.Now().Add(-time.Duration(seconds) * time.Second)
	aggregated, err := discovery.AggregatedSince(discoveryMetrics, refTime)
	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: "Unable to generate aggregated discovery metrics"})
		return
	}
	// log.Debugf("DiscoveryMetricsAggregated data: %+v", aggregated)
	r.JSON(http.StatusOK, aggregated)
}

// DiscoveryQueueMetricsRaw returns the raw queue metrics (active and
// queued values), data taken secondly for the last N seconds.
func (this *HttpAPI) DiscoveryQueueMetricsRaw(params martini.Params, r render.Render, req *http.Request, user auth.User) {
	seconds, err := strconv.Atoi(params["seconds"])
	log.Debugf("DiscoveryQueueMetricsRaw: seconds: %d", seconds)
	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: "Unable to generate discovery queue  aggregated metrics"})
		return
	}

	queue := discovery.CreateOrReturnQueue("DEFAULT")
	metrics := queue.DiscoveryQueueMetrics(seconds)
	log.Debugf("DiscoveryQueueMetricsRaw data: %+v", metrics)

	r.JSON(http.StatusOK, metrics)
}

// DiscoveryQueueMetricsAggregated returns a single value showing the metrics of the discovery queue over the last N seconds.
// This is expected to be called every 60 seconds (?) and the config setting of the retention period is currently hard-coded.
// See go/discovery/ for more information.
func (this *HttpAPI) DiscoveryQueueMetricsAggregated(params martini.Params, r render.Render, req *http.Request, user auth.User) {
	seconds, err := strconv.Atoi(params["seconds"])
	log.Debugf("DiscoveryQueueMetricsAggregated: seconds: %d", seconds)
	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: "Unable to generate discovery queue aggregated metrics"})
		return
	}

	queue := discovery.CreateOrReturnQueue("DEFAULT")
	aggregated := queue.AggregatedDiscoveryQueueMetrics(seconds)
	log.Debugf("DiscoveryQueueMetricsAggregated data: %+v", aggregated)

	r.JSON(http.StatusOK, aggregated)
}

// BackendQueryMetricsRaw returns the raw backend query metrics
func (this *HttpAPI) BackendQueryMetricsRaw(params martini.Params, r render.Render, req *http.Request, user auth.User) {
	seconds, err := strconv.Atoi(params["seconds"])
	log.Debugf("BackendQueryMetricsRaw: seconds: %d", seconds)
	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: "Unable to generate raw backend query metrics"})
		return
	}

	refTime := time.Now().Add(-time.Duration(seconds) * time.Second)
	m, err := queryMetrics.Since(refTime)
	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: "Unable to return backend query metrics"})
		return
	}

	log.Debugf("BackendQueryMetricsRaw data: %+v", m)

	r.JSON(http.StatusOK, m)
}

func (this *HttpAPI) BackendQueryMetricsAggregated(params martini.Params, r render.Render, req *http.Request, user auth.User) {
	seconds, err := strconv.Atoi(params["seconds"])
	log.Debugf("BackendQueryMetricsAggregated: seconds: %d", seconds)
	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: "Unable to aggregated generate backend query metrics"})
		return
	}

	refTime := time.Now().Add(-time.Duration(seconds) * time.Second)
	aggregated := query.AggregatedSince(queryMetrics, refTime)
	log.Debugf("BackendQueryMetricsAggregated data: %+v", aggregated)

	r.JSON(http.StatusOK, aggregated)
}

// WriteBufferMetricsRaw returns the raw instance write buffer metrics
func (this *HttpAPI) WriteBufferMetricsRaw(params martini.Params, r render.Render, req *http.Request, user auth.User) {
	seconds, err := strconv.Atoi(params["seconds"])
	log.Debugf("WriteBufferMetricsRaw: seconds: %d", seconds)
	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: "Unable to generate raw instance write buffer metrics"})
		return
	}

	refTime := time.Now().Add(-time.Duration(seconds) * time.Second)
	m, err := writeBufferMetrics.Since(refTime)
	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: "Unable to return instance write buffermetrics"})
		return
	}

	log.Debugf("WriteBufferMetricsRaw data: %+v", m)

	r.JSON(http.StatusOK, m)
}

// WriteBufferMetricsAggregated provides aggregate metrics of instance write buffer metrics
func (this *HttpAPI) WriteBufferMetricsAggregated(params martini.Params, r render.Render, req *http.Request, user auth.User) {
	seconds, err := strconv.Atoi(params["seconds"])
	log.Debugf("WriteBufferMetricsAggregated: seconds: %d", seconds)
	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: "Unable to aggregated instance write buffer metrics"})
		return
	}

	refTime := time.Now().Add(-time.Duration(seconds) * time.Second)
	aggregated := inst.AggregatedSince(writeBufferMetrics, refTime)
	log.Debugf("WriteBufferMetricsAggregated data: %+v", aggregated)

	r.JSON(http.StatusOK, aggregated)
}

// Headers is a self-test call which returns HTTP headers
func (this *HttpAPI) Headers(params martini.Params, r render.Render, req *http.Request) {
	r.JSON(http.StatusOK, req.Header)
}

// Health performs a self test
func (this *HttpAPI) Health(params martini.Params, r render.Render, req *http.Request) {
	health, err := process.HealthTest()
	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: fmt.Sprintf("Application node is unhealthy %+v", err), Details: health})
		return
	}

	Respond(r, &APIResponse{Code: OK, Message: "Application node is healthy", Details: health})

}

// LBCheck returns a constant respnse, and this can be used by load balancers that expect a given string.
func (this *HttpAPI) LBCheck(params martini.Params, r render.Render, req *http.Request) {
	r.JSON(http.StatusOK, "OK")
}

// LBCheck returns a constant respnse, and this can be used by load balancers that expect a given string.
func (this *HttpAPI) LeaderCheck(params martini.Params, r render.Render, req *http.Request) {
	respondStatus, err := strconv.Atoi(params["errorStatusCode"])
	if err != nil || respondStatus < 0 {
		respondStatus = http.StatusNotFound
	}

	if logic.IsLeader() {
		r.JSON(http.StatusOK, "OK")
	} else {
		r.JSON(respondStatus, "Not leader")
	}
}

// A configurable endpoint that can be for regular status checks or whatever.  While similar to
// Health() this returns 500 on failure.  This will prevent issues for those that have come to
// expect a 200
// It might be a good idea to deprecate the current Health() behavior and roll this in at some
// point
func (this *HttpAPI) StatusCheck(params martini.Params, r render.Render, req *http.Request) {
	health, err := process.HealthTest()
	if err != nil {
		r.JSON(500, &APIResponse{Code: ERROR, Message: fmt.Sprintf("Application node is unhealthy %+v", err), Details: health})
		return
	}
	Respond(r, &APIResponse{Code: OK, Message: "Application node is healthy", Details: health})
}

// GrabElection forcibly grabs leadership. Use with care!!
func (this *HttpAPI) GrabElection(params martini.Params, r render.Render, req *http.Request, user auth.User) {
	if !isAuthorizedForAction(req, user) {
		Respond(r, &APIResponse{Code: ERROR, Message: "Unauthorized"})
		return
	}
	err := process.GrabElection()
	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: fmt.Sprintf("Unable to grab election: %+v", err)})
		return
	}

	Respond(r, &APIResponse{Code: OK, Message: "Node elected as leader"})
}

// Reelect causes re-elections for an active node
func (this *HttpAPI) Reelect(params martini.Params, r render.Render, req *http.Request, user auth.User) {
	if !isAuthorizedForAction(req, user) {
		Respond(r, &APIResponse{Code: ERROR, Message: "Unauthorized"})
		return
	}
	err := process.Reelect()
	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: fmt.Sprintf("Unable to re-elect: %+v", err)})
		return
	}

	Respond(r, &APIResponse{Code: OK, Message: "Set re-elections"})
}

// ReloadConfiguration reloads confiug settings (not all of which will apply after change)
func (this *HttpAPI) ReloadConfiguration(params martini.Params, r render.Render, req *http.Request, user auth.User) {
	if !isAuthorizedForAction(req, user) {
		Respond(r, &APIResponse{Code: ERROR, Message: "Unauthorized"})
		return
	}
	extraConfigFile := req.URL.Query().Get("config")
	config.Reload(extraConfigFile)
	inst.AuditOperation("reload-configuration", nil, "Triggered via API")

	Respond(r, &APIResponse{Code: OK, Message: "Config reloaded", Details: extraConfigFile})
}

// ReplicationAnalysis retuens list of issues
func (this *HttpAPI) replicationAnalysis(clusterName string, instanceKey *inst.InstanceKey, params martini.Params, r render.Render, req *http.Request) {
	analysis, err := inst.GetReplicationAnalysis(clusterName, &inst.ReplicationAnalysisHints{IncludeDowntimed: true})
	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: fmt.Sprintf("Cannot get analysis: %+v", err)})
		return
	}
	// Possibly filter single instance
	if instanceKey != nil {
		filtered := analysis[:0]
		for _, analysisEntry := range analysis {
			if instanceKey.Equals(&analysisEntry.AnalyzedInstanceKey) {
				filtered = append(filtered, analysisEntry)
			}
		}
		analysis = filtered
	}

	Respond(r, &APIResponse{Code: OK, Message: "Analysis", Details: analysis})
}

// ReplicationAnalysis retuens list of issues
func (this *HttpAPI) ReplicationAnalysis(params martini.Params, r render.Render, req *http.Request) {
	this.replicationAnalysis("", nil, params, r, req)
}

// ReplicationAnalysis retuens list of issues
func (this *HttpAPI) ReplicationAnalysisForCluster(params martini.Params, r render.Render, req *http.Request) {
	var clusterName string
	var err error
	if clusterName, err = inst.DeduceClusterName(params["clusterName"]); err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: fmt.Sprintf("Cannot get analysis: %+v", err)})
		return
	}
	if clusterName == "" {
		Respond(r, &APIResponse{Code: ERROR, Message: fmt.Sprintf("Cannot get cluster name: %+v", params["clusterName"])})
		return
	}
	this.replicationAnalysis(clusterName, nil, params, r, req)
}

// ReplicationAnalysis retuens list of issues
func (this *HttpAPI) ReplicationAnalysisForKey(params martini.Params, r render.Render, req *http.Request) {
	instanceKey, err := this.getInstanceKey(params["host"], params["port"])
	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: fmt.Sprintf("Cannot get analysis: %+v", err)})
		return
	}
	if !instanceKey.IsValid() {
		Respond(r, &APIResponse{Code: ERROR, Message: fmt.Sprintf("Cannot get analysis: invalid key %+v", instanceKey)})
		return
	}
	this.replicationAnalysis("", &instanceKey, params, r, req)
}

// RecoverLite attempts recovery on a given instance, without executing external processes
func (this *HttpAPI) RecoverLite(params martini.Params, r render.Render, req *http.Request, user auth.User) {
	params["skipProcesses"] = "true"
	this.Recover(params, r, req, user)
}

// Recover attempts recovery on a given instance
func (this *HttpAPI) Recover(params martini.Params, r render.Render, req *http.Request, user auth.User) {
	if !isAuthorizedForAction(req, user) {
		Respond(r, &APIResponse{Code: ERROR, Message: "Unauthorized"})
		return
	}
	instanceKey, err := this.getInstanceKey(params["host"], params["port"])
	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: err.Error()})
		return
	}
	var candidateKey *inst.InstanceKey
	if key, err := this.getInstanceKey(params["candidateHost"], params["candidatePort"]); err == nil {
		candidateKey = &key
	}

	skipProcesses := (req.URL.Query().Get("skipProcesses") == "true") || (params["skipProcesses"] == "true")
	recoveryAttempted, promotedInstanceKey, err := logic.CheckAndRecover(&instanceKey, candidateKey, skipProcesses)
	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: err.Error(), Details: instanceKey})
		return
	}
	if !recoveryAttempted {
		Respond(r, &APIResponse{Code: ERROR, Message: "Recovery not attempted", Details: instanceKey})
		return
	}
	if promotedInstanceKey == nil {
		Respond(r, &APIResponse{Code: ERROR, Message: "Recovery attempted but no instance promoted", Details: instanceKey})
		return
	}
	Respond(r, &APIResponse{Code: OK, Message: fmt.Sprintf("Recovery executed on %+v", instanceKey), Details: *promotedInstanceKey})
}

// GracefulPrimaryTakeover gracefully fails over a primary onto its single replica.
func (this *HttpAPI) gracefulPrimaryTakeover(params martini.Params, r render.Render, req *http.Request, user auth.User) {
	if !isAuthorizedForAction(req, user) {
		Respond(r, &APIResponse{Code: ERROR, Message: "Unauthorized"})
		return
	}
	clusterName, err := figureClusterName(getClusterHint(params))
	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: err.Error()})
		return
	}
	designatedKey, _ := this.getInstanceKey(params["designatedHost"], params["designatedPort"])
	// designatedKey may be empty/invalid
	topologyRecovery, err := logic.GracefulPrimaryTakeover(clusterName, &designatedKey)
	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: err.Error(), Details: topologyRecovery})
		return
	}
	if topologyRecovery == nil || topologyRecovery.SuccessorKey == nil {
		Respond(r, &APIResponse{Code: ERROR, Message: "graceful-primary-takeover: no successor promoted", Details: topologyRecovery})
		return
	}
	Respond(r, &APIResponse{Code: OK, Message: "graceful-primary-takeover: successor promoted", Details: topologyRecovery})
}

// GracefulPrimaryTakeover gracefully fails over a primary, either:
// - onto its single replica, or
// - onto a replica indicated by the user
func (this *HttpAPI) GracefulPrimaryTakeover(params martini.Params, r render.Render, req *http.Request, user auth.User) {
	this.gracefulPrimaryTakeover(params, r, req, user)
}

// GracefulPrimaryTakeoverAuto gracefully fails over a primary onto a replica of orchestrator's choosing
func (this *HttpAPI) GracefulPrimaryTakeoverAuto(params martini.Params, r render.Render, req *http.Request, user auth.User) {
	this.gracefulPrimaryTakeover(params, r, req, user)
}

// ForcePrimaryFailover fails over a primary (even if there's no particular problem with the primary)
func (this *HttpAPI) ForcePrimaryFailover(params martini.Params, r render.Render, req *http.Request, user auth.User) {
	if !isAuthorizedForAction(req, user) {
		Respond(r, &APIResponse{Code: ERROR, Message: "Unauthorized"})
		return
	}
	clusterName, err := figureClusterName(getClusterHint(params))
	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: err.Error()})
		return
	}
	topologyRecovery, err := logic.ForcePrimaryFailover(clusterName)
	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: err.Error()})
		return
	}
	if topologyRecovery.SuccessorKey != nil {
		Respond(r, &APIResponse{Code: OK, Message: "Primary failed over", Details: topologyRecovery})
	} else {
		Respond(r, &APIResponse{Code: ERROR, Message: "Primary not failed over", Details: topologyRecovery})
	}
}

// ForcePrimaryTakeover fails over a primary (even if there's no particular problem with the primary)
func (this *HttpAPI) ForcePrimaryTakeover(params martini.Params, r render.Render, req *http.Request, user auth.User) {
	if !isAuthorizedForAction(req, user) {
		Respond(r, &APIResponse{Code: ERROR, Message: "Unauthorized"})
		return
	}
	clusterName, err := figureClusterName(getClusterHint(params))
	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: err.Error()})
		return
	}
	designatedKey, _ := this.getInstanceKey(params["designatedHost"], params["designatedPort"])
	designatedInstance, _, err := inst.ReadInstance(&designatedKey)
	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: err.Error()})
		return
	}
	if designatedInstance == nil {
		Respond(r, &APIResponse{Code: ERROR, Message: "Instance not found"})
		return
	}

	topologyRecovery, err := logic.ForcePrimaryTakeover(clusterName, designatedInstance)
	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: err.Error()})
		return
	}
	if topologyRecovery.SuccessorKey != nil {
		Respond(r, &APIResponse{Code: OK, Message: "Primary failed over", Details: topologyRecovery})
	} else {
		Respond(r, &APIResponse{Code: ERROR, Message: "Primary not failed over", Details: topologyRecovery})
	}
}

// Registers promotion preference for given instance
func (this *HttpAPI) RegisterCandidate(params martini.Params, r render.Render, req *http.Request, user auth.User) {
	if !isAuthorizedForAction(req, user) {
		Respond(r, &APIResponse{Code: ERROR, Message: "Unauthorized"})
		return
	}
	instanceKey, err := this.getInstanceKey(params["host"], params["port"])
	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: err.Error()})
		return
	}
	promotionRule, err := promotionrule.Parse(params["promotionRule"])
	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: err.Error()})
		return
	}

	candidate := inst.NewCandidateDatabaseInstance(&instanceKey, promotionRule).WithCurrentTime()

	if orcraft.IsRaftEnabled() {
		_, err = orcraft.PublishCommand("register-candidate", candidate)
	} else {
		err = inst.RegisterCandidateInstance(candidate)
	}

	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: err.Error()})
		return
	}

	Respond(r, &APIResponse{Code: OK, Message: "Registered candidate", Details: instanceKey})
}

// AutomatedRecoveryFilters retuens list of clusters which are configured with automated recovery
func (this *HttpAPI) AutomatedRecoveryFilters(params martini.Params, r render.Render, req *http.Request) {
	automatedRecoveryMap := make(map[string]interface{})
	automatedRecoveryMap["RecoverPrimaryClusterFilters"] = config.Config.RecoverPrimaryClusterFilters
	automatedRecoveryMap["RecoverIntermediatePrimaryClusterFilters"] = config.Config.RecoverIntermediatePrimaryClusterFilters
	automatedRecoveryMap["RecoveryIgnoreHostnameFilters"] = config.Config.RecoveryIgnoreHostnameFilters

	Respond(r, &APIResponse{Code: OK, Message: "Automated recovery configuration details", Details: automatedRecoveryMap})
}

// AuditFailureDetection provides list of topology_failure_detection entries
func (this *HttpAPI) AuditFailureDetection(params martini.Params, r render.Render, req *http.Request) {

	var audits []*logic.TopologyRecovery
	var err error

	if detectionId, derr := strconv.ParseInt(params["id"], 10, 0); derr == nil && detectionId > 0 {
		audits, err = logic.ReadFailureDetection(detectionId)
	} else {
		page, derr := strconv.Atoi(params["page"])
		if derr != nil || page < 0 {
			page = 0
		}
		audits, err = logic.ReadRecentFailureDetections(params["clusterAlias"], page)
	}

	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: fmt.Sprintf("%+v", err)})
		return
	}

	r.JSON(http.StatusOK, audits)
}

// AuditRecoverySteps returns audited steps of a given recovery
func (this *HttpAPI) AuditRecoverySteps(params martini.Params, r render.Render, req *http.Request) {
	recoveryUID := params["uid"]
	audits, err := logic.ReadTopologyRecoverySteps(recoveryUID)

	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: fmt.Sprintf("%+v", err)})
		return
	}

	r.JSON(http.StatusOK, audits)
}

// ReadReplicationAnalysisChangelog lists instances and their analysis changelog
func (this *HttpAPI) ReadReplicationAnalysisChangelog(params martini.Params, r render.Render, req *http.Request) {
	changelogs, err := inst.ReadReplicationAnalysisChangelog()

	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: fmt.Sprintf("%+v", err)})
		return
	}

	r.JSON(http.StatusOK, changelogs)
}

// AuditRecovery provides list of topology-recovery entries
func (this *HttpAPI) AuditRecovery(params martini.Params, r render.Render, req *http.Request) {
	var audits []*logic.TopologyRecovery
	var err error

	if recoveryUID := params["uid"]; recoveryUID != "" {
		audits, err = logic.ReadRecoveryByUID(recoveryUID)
	} else if recoveryId, derr := strconv.ParseInt(params["id"], 10, 0); derr == nil && recoveryId > 0 {
		audits, err = logic.ReadRecovery(recoveryId)
	} else {
		page, derr := strconv.Atoi(params["page"])
		if derr != nil || page < 0 {
			page = 0
		}
		unacknowledgedOnly := (req.URL.Query().Get("unacknowledged") == "true")

		audits, err = logic.ReadRecentRecoveries(params["clusterName"], params["clusterAlias"], unacknowledgedOnly, page)
	}

	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: fmt.Sprintf("%+v", err)})
		return
	}

	r.JSON(http.StatusOK, audits)
}

// ActiveClusterRecovery returns recoveries in-progress for a given cluster
func (this *HttpAPI) ActiveClusterRecovery(params martini.Params, r render.Render, req *http.Request) {
	recoveries, err := logic.ReadActiveClusterRecovery(params["clusterName"])

	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: fmt.Sprintf("%+v", err)})
		return
	}

	r.JSON(http.StatusOK, recoveries)
}

// RecentlyActiveClusterRecovery returns recoveries in-progress for a given cluster
func (this *HttpAPI) RecentlyActiveClusterRecovery(params martini.Params, r render.Render, req *http.Request) {
	recoveries, err := logic.ReadRecentlyActiveClusterRecovery(params["clusterName"])

	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: fmt.Sprintf("%+v", err)})
		return
	}

	r.JSON(http.StatusOK, recoveries)
}

// RecentlyActiveClusterRecovery returns recoveries in-progress for a given cluster
func (this *HttpAPI) RecentlyActiveInstanceRecovery(params martini.Params, r render.Render, req *http.Request) {
	instanceKey, err := this.getInstanceKey(params["host"], params["port"])
	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: err.Error()})
		return
	}

	recoveries, err := logic.ReadRecentlyActiveInstanceRecovery(&instanceKey)

	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: fmt.Sprintf("%+v", err)})
		return
	}

	r.JSON(http.StatusOK, recoveries)
}

// ClusterInfo provides details of a given cluster
func (this *HttpAPI) AcknowledgeClusterRecoveries(params martini.Params, r render.Render, req *http.Request, user auth.User) {
	if !isAuthorizedForAction(req, user) {
		Respond(r, &APIResponse{Code: ERROR, Message: "Unauthorized"})
		return
	}

	var clusterName string
	var err error
	if params["clusterAlias"] != "" {
		clusterName, err = inst.GetClusterByAlias(params["clusterAlias"])
	} else {
		clusterName, err = figureClusterName(getClusterHint(params))
	}

	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: fmt.Sprintf("%+v", err)})
		return
	}

	comment := strings.TrimSpace(req.URL.Query().Get("comment"))
	if comment == "" {
		Respond(r, &APIResponse{Code: ERROR, Message: "No acknowledge comment given"})
		return
	}
	userId := getUserId(req, user)
	if userId == "" {
		userId = inst.GetMaintenanceOwner()
	}
	if orcraft.IsRaftEnabled() {
		ack := logic.NewRecoveryAcknowledgement(userId, comment)
		ack.ClusterName = clusterName
		_, err = orcraft.PublishCommand("ack-recovery", ack)
	} else {
		_, err = logic.AcknowledgeClusterRecoveries(clusterName, userId, comment)
	}
	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: fmt.Sprintf("%+v", err)})
		return
	}

	Respond(r, &APIResponse{Code: OK, Message: "Acknowledged cluster recoveries", Details: clusterName})
}

// ClusterInfo provides details of a given cluster
func (this *HttpAPI) AcknowledgeInstanceRecoveries(params martini.Params, r render.Render, req *http.Request, user auth.User) {
	if !isAuthorizedForAction(req, user) {
		Respond(r, &APIResponse{Code: ERROR, Message: "Unauthorized"})
		return
	}

	instanceKey, err := this.getInstanceKey(params["host"], params["port"])
	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: err.Error()})
		return
	}

	comment := strings.TrimSpace(req.URL.Query().Get("comment"))
	if comment == "" {
		Respond(r, &APIResponse{Code: ERROR, Message: "No acknowledge comment given"})
		return
	}
	userId := getUserId(req, user)
	if userId == "" {
		userId = inst.GetMaintenanceOwner()
	}
	if orcraft.IsRaftEnabled() {
		ack := logic.NewRecoveryAcknowledgement(userId, comment)
		ack.Key = instanceKey
		_, err = orcraft.PublishCommand("ack-recovery", ack)
	} else {
		_, err = logic.AcknowledgeInstanceRecoveries(&instanceKey, userId, comment)
	}
	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: fmt.Sprintf("%+v", err)})
		return
	}

	Respond(r, &APIResponse{Code: OK, Message: "Acknowledged instance recoveries", Details: instanceKey})
}

// ClusterInfo provides details of a given cluster
func (this *HttpAPI) AcknowledgeRecovery(params martini.Params, r render.Render, req *http.Request, user auth.User) {
	if !isAuthorizedForAction(req, user) {
		Respond(r, &APIResponse{Code: ERROR, Message: "Unauthorized"})
		return
	}
	var err error
	var recoveryId int64
	var idParam string

	// Ack either via id or uid
	recoveryUid := params["uid"]
	if recoveryUid == "" {
		idParam = params["recoveryId"]
		recoveryId, err = strconv.ParseInt(idParam, 10, 0)
		if err != nil {
			Respond(r, &APIResponse{Code: ERROR, Message: err.Error()})
			return
		}
	} else {
		idParam = recoveryUid
	}
	comment := strings.TrimSpace(req.URL.Query().Get("comment"))
	if comment == "" {
		Respond(r, &APIResponse{Code: ERROR, Message: "No acknowledge comment given"})
		return
	}
	userId := getUserId(req, user)
	if userId == "" {
		userId = inst.GetMaintenanceOwner()
	}
	if orcraft.IsRaftEnabled() {
		ack := logic.NewRecoveryAcknowledgement(userId, comment)
		ack.Id = recoveryId
		ack.UID = recoveryUid
		_, err = orcraft.PublishCommand("ack-recovery", ack)
	} else {
		if recoveryUid != "" {
			_, err = logic.AcknowledgeRecoveryByUID(recoveryUid, userId, comment)
		} else {
			_, err = logic.AcknowledgeRecovery(recoveryId, userId, comment)
		}
	}

	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: fmt.Sprintf("%+v", err)})
		return
	}

	Respond(r, &APIResponse{Code: OK, Message: "Acknowledged recovery", Details: idParam})
}

// ClusterInfo provides details of a given cluster
func (this *HttpAPI) AcknowledgeAllRecoveries(params martini.Params, r render.Render, req *http.Request, user auth.User) {
	if !isAuthorizedForAction(req, user) {
		Respond(r, &APIResponse{Code: ERROR, Message: "Unauthorized"})
		return
	}

	comment := strings.TrimSpace(req.URL.Query().Get("comment"))
	if comment == "" {
		Respond(r, &APIResponse{Code: ERROR, Message: "No acknowledge comment given"})
		return
	}
	userId := getUserId(req, user)
	if userId == "" {
		userId = inst.GetMaintenanceOwner()
	}
	var err error
	if orcraft.IsRaftEnabled() {
		ack := logic.NewRecoveryAcknowledgement(userId, comment)
		ack.AllRecoveries = true
		_, err = orcraft.PublishCommand("ack-recovery", ack)
	} else {
		_, err = logic.AcknowledgeAllRecoveries(userId, comment)
	}
	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: fmt.Sprintf("%+v", err)})
		return
	}

	Respond(r, &APIResponse{Code: OK, Message: "Acknowledged all recoveries", Details: comment})
}

// BlockedRecoveries reads list of currently blocked recoveries, optionally filtered by cluster name
func (this *HttpAPI) BlockedRecoveries(params martini.Params, r render.Render, req *http.Request) {
	blockedRecoveries, err := logic.ReadBlockedRecoveries(params["clusterName"])

	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: fmt.Sprintf("%+v", err)})
		return
	}

	r.JSON(http.StatusOK, blockedRecoveries)
}

// DisableGlobalRecoveries globally disables recoveries
func (this *HttpAPI) DisableGlobalRecoveries(params martini.Params, r render.Render, req *http.Request, user auth.User) {
	if !isAuthorizedForAction(req, user) {
		Respond(r, &APIResponse{Code: ERROR, Message: "Unauthorized"})
		return
	}

	var err error
	if orcraft.IsRaftEnabled() {
		_, err = orcraft.PublishCommand("disable-global-recoveries", 0)
	} else {
		err = logic.DisableRecovery()
	}

	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: fmt.Sprintf("%+v", err)})
		return
	}

	Respond(r, &APIResponse{Code: OK, Message: "Globally disabled recoveries", Details: "disabled"})
}

// EnableGlobalRecoveries globally enables recoveries
func (this *HttpAPI) EnableGlobalRecoveries(params martini.Params, r render.Render, req *http.Request, user auth.User) {
	if !isAuthorizedForAction(req, user) {
		Respond(r, &APIResponse{Code: ERROR, Message: "Unauthorized"})
		return
	}

	var err error
	if orcraft.IsRaftEnabled() {
		_, err = orcraft.PublishCommand("enable-global-recoveries", 0)
	} else {
		err = logic.EnableRecovery()
	}
	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: fmt.Sprintf("%+v", err)})
		return
	}

	Respond(r, &APIResponse{Code: OK, Message: "Globally enabled recoveries", Details: "enabled"})
}

// CheckGlobalRecoveries checks whether
func (this *HttpAPI) CheckGlobalRecoveries(params martini.Params, r render.Render, req *http.Request) {
	isDisabled, err := logic.IsRecoveryDisabled()

	if err != nil {
		Respond(r, &APIResponse{Code: ERROR, Message: fmt.Sprintf("%+v", err)})
		return
	}
	details := "enabled"
	if isDisabled {
		details = "disabled"
	}
	Respond(r, &APIResponse{Code: OK, Message: fmt.Sprintf("Global recoveries %+v", details), Details: details})
}

func (this *HttpAPI) registerSingleAPIRequest(m *martini.ClassicMartini, path string, handler martini.Handler, allowProxy bool) {
	registeredPaths = append(registeredPaths, path)
	fullPath := fmt.Sprintf("%s/api/%s", this.URLPrefix, path)

	m.Get(fullPath, handler)
}

func (this *HttpAPI) registerAPIRequestInternal(m *martini.ClassicMartini, path string, handler martini.Handler, allowProxy bool) {
	this.registerSingleAPIRequest(m, path, handler, allowProxy)
}

func (this *HttpAPI) registerAPIRequest(m *martini.ClassicMartini, path string, handler martini.Handler) {
	this.registerAPIRequestInternal(m, path, handler, true)
}

func (this *HttpAPI) registerAPIRequestNoProxy(m *martini.ClassicMartini, path string, handler martini.Handler) {
	this.registerAPIRequestInternal(m, path, handler, false)
}

// RegisterRequests makes for the de-facto list of known API calls
func (this *HttpAPI) RegisterRequests(m *martini.ClassicMartini) {
	// Smart relocation:
	this.registerAPIRequest(m, "relocate/:host/:port/:belowHost/:belowPort", this.RelocateBelow)
	this.registerAPIRequest(m, "relocate-below/:host/:port/:belowHost/:belowPort", this.RelocateBelow)
	this.registerAPIRequest(m, "relocate-replicas/:host/:port/:belowHost/:belowPort", this.RelocateReplicas)
	this.registerAPIRequest(m, "regroup-replicas/:host/:port", this.RegroupReplicas)

	// Classic file:pos relocation:
	this.registerAPIRequest(m, "move-up/:host/:port", this.MoveUp)
	this.registerAPIRequest(m, "move-up-replicas/:host/:port", this.MoveUpReplicas)
	this.registerAPIRequest(m, "move-below/:host/:port/:siblingHost/:siblingPort", this.MoveBelow)
	this.registerAPIRequest(m, "repoint/:host/:port/:belowHost/:belowPort", this.Repoint)
	this.registerAPIRequest(m, "repoint-replicas/:host/:port", this.RepointReplicas)
	this.registerAPIRequest(m, "make-co-primary/:host/:port", this.MakeCoPrimary)
	this.registerAPIRequest(m, "take-siblings/:host/:port", this.TakeSiblings)
	this.registerAPIRequest(m, "take-primary/:host/:port", this.TakePrimary)

	// Binlog server relocation:
	this.registerAPIRequest(m, "regroup-replicas-bls/:host/:port", this.RegroupReplicasBinlogServers)

	// GTID relocation:
	this.registerAPIRequest(m, "move-below-gtid/:host/:port/:belowHost/:belowPort", this.MoveBelowGTID)
	this.registerAPIRequest(m, "move-replicas-gtid/:host/:port/:belowHost/:belowPort", this.MoveReplicasGTID)
	this.registerAPIRequest(m, "regroup-replicas-gtid/:host/:port", this.RegroupReplicasGTID)

	// Replication, general:
	this.registerAPIRequest(m, "enable-gtid/:host/:port", this.EnableGTID)
	this.registerAPIRequest(m, "disable-gtid/:host/:port", this.DisableGTID)
	this.registerAPIRequest(m, "locate-gtid-errant/:host/:port", this.LocateErrantGTID)
	this.registerAPIRequest(m, "gtid-errant-reset-primary/:host/:port", this.ErrantGTIDResetPrimary)
	this.registerAPIRequest(m, "gtid-errant-inject-empty/:host/:port", this.ErrantGTIDInjectEmpty)
	this.registerAPIRequest(m, "skip-query/:host/:port", this.SkipQuery)
	this.registerAPIRequest(m, "start-replica/:host/:port", this.StartReplication)
	this.registerAPIRequest(m, "restart-replica/:host/:port", this.RestartReplication)
	this.registerAPIRequest(m, "stop-replica/:host/:port", this.StopReplication)
	this.registerAPIRequest(m, "stop-replica-nice/:host/:port", this.StopReplicationNicely)
	this.registerAPIRequest(m, "reset-replica/:host/:port", this.ResetReplication)
	this.registerAPIRequest(m, "detach-replica/:host/:port", this.DetachReplicaPrimaryHost)
	this.registerAPIRequest(m, "reattach-replica/:host/:port", this.ReattachReplicaPrimaryHost)
	this.registerAPIRequest(m, "detach-replica-primary-host/:host/:port", this.DetachReplicaPrimaryHost)
	this.registerAPIRequest(m, "reattach-replica-primary-host/:host/:port", this.ReattachReplicaPrimaryHost)
	this.registerAPIRequest(m, "flush-binary-logs/:host/:port", this.FlushBinaryLogs)
	this.registerAPIRequest(m, "purge-binary-logs/:host/:port/:logFile", this.PurgeBinaryLogs)
	this.registerAPIRequest(m, "restart-replica-statements/:host/:port", this.RestartReplicationStatements)

	// Replication information:
	this.registerAPIRequest(m, "can-replicate-from/:host/:port/:belowHost/:belowPort", this.CanReplicateFrom)
	this.registerAPIRequest(m, "can-replicate-from-gtid/:host/:port/:belowHost/:belowPort", this.CanReplicateFromGTID)

	// Instance:
	this.registerAPIRequest(m, "set-read-only/:host/:port", this.SetReadOnly)
	this.registerAPIRequest(m, "set-writeable/:host/:port", this.SetWriteable)
	this.registerAPIRequest(m, "kill-query/:host/:port/:process", this.KillQuery)

	// Pools:
	this.registerAPIRequest(m, "submit-pool-instances/:pool", this.SubmitPoolInstances)
	this.registerAPIRequest(m, "cluster-pool-instances/:clusterName", this.ReadClusterPoolInstancesMap)
	this.registerAPIRequest(m, "cluster-pool-instances/:clusterName/:pool", this.ReadClusterPoolInstancesMap)
	this.registerAPIRequest(m, "heuristic-cluster-pool-instances/:clusterName", this.GetHeuristicClusterPoolInstances)
	this.registerAPIRequest(m, "heuristic-cluster-pool-instances/:clusterName/:pool", this.GetHeuristicClusterPoolInstances)
	this.registerAPIRequest(m, "heuristic-cluster-pool-lag/:clusterName", this.GetHeuristicClusterPoolInstancesLag)
	this.registerAPIRequest(m, "heuristic-cluster-pool-lag/:clusterName/:pool", this.GetHeuristicClusterPoolInstancesLag)

	// Information:
	this.registerAPIRequest(m, "search/:searchString", this.Search)
	this.registerAPIRequest(m, "search", this.Search)

	// Cluster
	this.registerAPIRequest(m, "cluster/:clusterHint", this.Cluster)
	this.registerAPIRequest(m, "cluster/alias/:clusterAlias", this.ClusterByAlias)
	this.registerAPIRequest(m, "cluster/instance/:host/:port", this.ClusterByInstance)
	this.registerAPIRequest(m, "cluster-info/:clusterHint", this.ClusterInfo)
	this.registerAPIRequest(m, "cluster-info/alias/:clusterAlias", this.ClusterInfoByAlias)
	this.registerAPIRequest(m, "cluster-osc-replicas/:clusterHint", this.ClusterOSCReplicas)
	this.registerAPIRequest(m, "set-cluster-alias/:clusterName", this.SetClusterAliasManualOverride)
	this.registerAPIRequest(m, "clusters", this.Clusters)
	this.registerAPIRequest(m, "clusters-info", this.ClustersInfo)

	this.registerAPIRequest(m, "primaries", this.Primaries)
	this.registerAPIRequest(m, "primary/:clusterHint", this.ClusterPrimary)
	this.registerAPIRequest(m, "instance-replicas/:host/:port", this.InstanceReplicas)
	this.registerAPIRequest(m, "all-instances", this.AllInstances)
	this.registerAPIRequest(m, "downtimed", this.Downtimed)
	this.registerAPIRequest(m, "downtimed/:clusterHint", this.Downtimed)
	this.registerAPIRequest(m, "topology/:clusterHint", this.AsciiTopology)
	this.registerAPIRequest(m, "topology/:host/:port", this.AsciiTopology)
	this.registerAPIRequest(m, "topology-tabulated/:clusterHint", this.AsciiTopologyTabulated)
	this.registerAPIRequest(m, "topology-tabulated/:host/:port", this.AsciiTopologyTabulated)
	this.registerAPIRequest(m, "topology-tags/:clusterHint", this.AsciiTopologyTags)
	this.registerAPIRequest(m, "topology-tags/:host/:port", this.AsciiTopologyTags)
	this.registerAPIRequest(m, "snapshot-topologies", this.SnapshotTopologies)

	// Key-value:
	this.registerAPIRequest(m, "submit-primaries-to-kv-stores", this.SubmitPrimariesToKvStores)
	this.registerAPIRequest(m, "submit-primaries-to-kv-stores/:clusterHint", this.SubmitPrimariesToKvStores)

	// Tags:
	this.registerAPIRequest(m, "tagged", this.Tagged)
	this.registerAPIRequest(m, "tags/:host/:port", this.Tags)
	this.registerAPIRequest(m, "tag-value/:host/:port", this.TagValue)
	this.registerAPIRequest(m, "tag-value/:host/:port/:tagName", this.TagValue)
	this.registerAPIRequest(m, "tag/:host/:port", this.Tag)
	this.registerAPIRequest(m, "tag/:host/:port/:tagName/:tagValue", this.Tag)
	this.registerAPIRequest(m, "untag/:host/:port", this.Untag)
	this.registerAPIRequest(m, "untag/:host/:port/:tagName", this.Untag)
	this.registerAPIRequest(m, "untag-all", this.UntagAll)
	this.registerAPIRequest(m, "untag-all/:tagName/:tagValue", this.UntagAll)

	// Instance management:
	this.registerAPIRequest(m, "instance/:host/:port", this.Instance)
	this.registerAPIRequest(m, "discover/:host/:port", this.Discover)
	this.registerAPIRequest(m, "async-discover/:host/:port", this.AsyncDiscover)
	this.registerAPIRequest(m, "refresh/:host/:port", this.Refresh)
	this.registerAPIRequest(m, "forget/:host/:port", this.Forget)
	this.registerAPIRequest(m, "forget-cluster/:clusterHint", this.ForgetCluster)
	this.registerAPIRequest(m, "begin-maintenance/:host/:port/:owner/:reason", this.BeginMaintenance)
	this.registerAPIRequest(m, "end-maintenance/:host/:port", this.EndMaintenanceByInstanceKey)
	this.registerAPIRequest(m, "in-maintenance/:host/:port", this.InMaintenance)
	this.registerAPIRequest(m, "end-maintenance/:maintenanceKey", this.EndMaintenance)
	this.registerAPIRequest(m, "maintenance", this.Maintenance)
	this.registerAPIRequest(m, "begin-downtime/:host/:port/:owner/:reason", this.BeginDowntime)
	this.registerAPIRequest(m, "begin-downtime/:host/:port/:owner/:reason/:duration", this.BeginDowntime)
	this.registerAPIRequest(m, "end-downtime/:host/:port", this.EndDowntime)

	// Recovery:
	this.registerAPIRequest(m, "replication-analysis", this.ReplicationAnalysis)
	this.registerAPIRequest(m, "replication-analysis/:clusterName", this.ReplicationAnalysisForCluster)
	this.registerAPIRequest(m, "replication-analysis/instance/:host/:port", this.ReplicationAnalysisForKey)
	this.registerAPIRequest(m, "recover/:host/:port", this.Recover)
	this.registerAPIRequest(m, "recover/:host/:port/:candidateHost/:candidatePort", this.Recover)
	this.registerAPIRequest(m, "recover-lite/:host/:port", this.RecoverLite)
	this.registerAPIRequest(m, "recover-lite/:host/:port/:candidateHost/:candidatePort", this.RecoverLite)
	this.registerAPIRequest(m, "graceful-primary-takeover/:host/:port", this.GracefulPrimaryTakeover)
	this.registerAPIRequest(m, "graceful-primary-takeover/:host/:port/:designatedHost/:designatedPort", this.GracefulPrimaryTakeover)
	this.registerAPIRequest(m, "graceful-primary-takeover/:clusterHint", this.GracefulPrimaryTakeover)
	this.registerAPIRequest(m, "graceful-primary-takeover/:clusterHint/:designatedHost/:designatedPort", this.GracefulPrimaryTakeover)
	this.registerAPIRequest(m, "graceful-primary-takeover-auto/:host/:port", this.GracefulPrimaryTakeoverAuto)
	this.registerAPIRequest(m, "graceful-primary-takeover-auto/:host/:port/:designatedHost/:designatedPort", this.GracefulPrimaryTakeoverAuto)
	this.registerAPIRequest(m, "graceful-primary-takeover-auto/:clusterHint", this.GracefulPrimaryTakeoverAuto)
	this.registerAPIRequest(m, "graceful-primary-takeover-auto/:clusterHint/:designatedHost/:designatedPort", this.GracefulPrimaryTakeoverAuto)
	this.registerAPIRequest(m, "force-primary-failover/:host/:port", this.ForcePrimaryFailover)
	this.registerAPIRequest(m, "force-primary-failover/:clusterHint", this.ForcePrimaryFailover)
	this.registerAPIRequest(m, "force-primary-takeover/:clusterHint/:designatedHost/:designatedPort", this.ForcePrimaryTakeover)
	this.registerAPIRequest(m, "force-primary-takeover/:host/:port/:designatedHost/:designatedPort", this.ForcePrimaryTakeover)
	this.registerAPIRequest(m, "register-candidate/:host/:port/:promotionRule", this.RegisterCandidate)
	this.registerAPIRequest(m, "automated-recovery-filters", this.AutomatedRecoveryFilters)
	this.registerAPIRequest(m, "audit-failure-detection", this.AuditFailureDetection)
	this.registerAPIRequest(m, "audit-failure-detection/:page", this.AuditFailureDetection)
	this.registerAPIRequest(m, "audit-failure-detection/id/:id", this.AuditFailureDetection)
	this.registerAPIRequest(m, "audit-failure-detection/alias/:clusterAlias", this.AuditFailureDetection)
	this.registerAPIRequest(m, "audit-failure-detection/alias/:clusterAlias/:page", this.AuditFailureDetection)
	this.registerAPIRequest(m, "replication-analysis-changelog", this.ReadReplicationAnalysisChangelog)
	this.registerAPIRequest(m, "audit-recovery", this.AuditRecovery)
	this.registerAPIRequest(m, "audit-recovery/:page", this.AuditRecovery)
	this.registerAPIRequest(m, "audit-recovery/id/:id", this.AuditRecovery)
	this.registerAPIRequest(m, "audit-recovery/uid/:uid", this.AuditRecovery)
	this.registerAPIRequest(m, "audit-recovery/cluster/:clusterName", this.AuditRecovery)
	this.registerAPIRequest(m, "audit-recovery/cluster/:clusterName/:page", this.AuditRecovery)
	this.registerAPIRequest(m, "audit-recovery/alias/:clusterAlias", this.AuditRecovery)
	this.registerAPIRequest(m, "audit-recovery/alias/:clusterAlias/:page", this.AuditRecovery)
	this.registerAPIRequest(m, "audit-recovery-steps/:uid", this.AuditRecoverySteps)
	this.registerAPIRequest(m, "active-cluster-recovery/:clusterName", this.ActiveClusterRecovery)
	this.registerAPIRequest(m, "recently-active-cluster-recovery/:clusterName", this.RecentlyActiveClusterRecovery)
	this.registerAPIRequest(m, "recently-active-instance-recovery/:host/:port", this.RecentlyActiveInstanceRecovery)
	this.registerAPIRequest(m, "ack-recovery/cluster/:clusterHint", this.AcknowledgeClusterRecoveries)
	this.registerAPIRequest(m, "ack-recovery/cluster/alias/:clusterAlias", this.AcknowledgeClusterRecoveries)
	this.registerAPIRequest(m, "ack-recovery/instance/:host/:port", this.AcknowledgeInstanceRecoveries)
	this.registerAPIRequest(m, "ack-recovery/:recoveryId", this.AcknowledgeRecovery)
	this.registerAPIRequest(m, "ack-recovery/uid/:uid", this.AcknowledgeRecovery)
	this.registerAPIRequest(m, "ack-all-recoveries", this.AcknowledgeAllRecoveries)
	this.registerAPIRequest(m, "blocked-recoveries", this.BlockedRecoveries)
	this.registerAPIRequest(m, "blocked-recoveries/cluster/:clusterName", this.BlockedRecoveries)
	this.registerAPIRequest(m, "disable-global-recoveries", this.DisableGlobalRecoveries)
	this.registerAPIRequest(m, "enable-global-recoveries", this.EnableGlobalRecoveries)
	this.registerAPIRequest(m, "check-global-recoveries", this.CheckGlobalRecoveries)

	// General
	this.registerAPIRequest(m, "problems", this.Problems)
	this.registerAPIRequest(m, "problems/:clusterName", this.Problems)
	this.registerAPIRequest(m, "audit", this.Audit)
	this.registerAPIRequest(m, "audit/:page", this.Audit)
	this.registerAPIRequest(m, "audit/instance/:host/:port", this.Audit)
	this.registerAPIRequest(m, "audit/instance/:host/:port/:page", this.Audit)
	this.registerAPIRequest(m, "resolve/:host/:port", this.Resolve)

	// Meta, no proxy
	this.registerAPIRequestNoProxy(m, "headers", this.Headers)
	this.registerAPIRequestNoProxy(m, "health", this.Health)
	this.registerAPIRequestNoProxy(m, "lb-check", this.LBCheck)
	this.registerAPIRequestNoProxy(m, "_ping", this.LBCheck)
	this.registerAPIRequestNoProxy(m, "leader-check", this.LeaderCheck)
	this.registerAPIRequestNoProxy(m, "leader-check/:errorStatusCode", this.LeaderCheck)
	this.registerAPIRequestNoProxy(m, "grab-election", this.GrabElection)
	this.registerAPIRequestNoProxy(m, "reload-configuration", this.ReloadConfiguration)
	this.registerAPIRequestNoProxy(m, "hostname-resolve-cache", this.HostnameResolveCache)
	this.registerAPIRequestNoProxy(m, "reset-hostname-resolve-cache", this.ResetHostnameResolveCache)
	// Meta
	this.registerAPIRequest(m, "routed-leader-check", this.LeaderCheck)
	this.registerAPIRequest(m, "reelect", this.Reelect)
	this.registerAPIRequest(m, "reload-cluster-alias", this.ReloadClusterAlias)
	this.registerAPIRequest(m, "deregister-hostname-unresolve/:host/:port", this.DeregisterHostnameUnresolve)
	this.registerAPIRequest(m, "register-hostname-unresolve/:host/:port/:virtualname", this.RegisterHostnameUnresolve)

	// Bulk access to information
	this.registerAPIRequest(m, "bulk-instances", this.BulkInstances)
	this.registerAPIRequest(m, "bulk-promotion-rules", this.BulkPromotionRules)

	// Monitoring
	this.registerAPIRequest(m, "discovery-metrics-raw/:seconds", this.DiscoveryMetricsRaw)
	this.registerAPIRequest(m, "discovery-metrics-aggregated/:seconds", this.DiscoveryMetricsAggregated)
	this.registerAPIRequest(m, "discovery-queue-metrics-raw/:seconds", this.DiscoveryQueueMetricsRaw)
	this.registerAPIRequest(m, "discovery-queue-metrics-aggregated/:seconds", this.DiscoveryQueueMetricsAggregated)
	this.registerAPIRequest(m, "backend-query-metrics-raw/:seconds", this.BackendQueryMetricsRaw)
	this.registerAPIRequest(m, "backend-query-metrics-aggregated/:seconds", this.BackendQueryMetricsAggregated)
	this.registerAPIRequest(m, "write-buffer-metrics-raw/:seconds", this.WriteBufferMetricsRaw)
	this.registerAPIRequest(m, "write-buffer-metrics-aggregated/:seconds", this.WriteBufferMetricsAggregated)

	// Configurable status check endpoint
	if config.Config.StatusEndpoint == config.DefaultStatusAPIEndpoint {
		this.registerAPIRequestNoProxy(m, "status", this.StatusCheck)
	} else {
		m.Get(config.Config.StatusEndpoint, this.StatusCheck)
	}
}
