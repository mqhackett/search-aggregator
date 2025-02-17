/*
IBM Confidential
OCO Source Materials
(C) Copyright IBM Corporation 2019 All Rights Reserved
The source code for this program is not published or otherwise divested of its trade secrets,
irrespective of what has been deposited with the U.S. Copyright Office.

Copyright (c) 2020 Red Hat, Inc.
*/
// Copyright Contributors to the Open Cluster Management project

package handlers

import (
	"fmt"
	"reflect"
	"strconv"
	"time"

	"github.com/golang/glog"
	db "github.com/open-cluster-management/search-aggregator/pkg/dbconnector"
	rg2 "github.com/redislabs/redisgraph-go"
)

func getEdgeUID(sourceUID string, edgeType string, destUID string) string {
	return fmt.Sprintf("%s-%s->%s", sourceUID, edgeType, destUID)
}

func resyncCluster(clusterName string, resources []*db.Resource, edges []db.Edge, metrics *SyncMetrics) (stats SyncResponse, err error) {
	glog.Info("Resync for cluster: ", clusterName, " edges to insert: ", len(edges))

	// First get the existing resources from the datastore for the cluster
	result, error := db.Store.Query(db.SanitizeQuery("MATCH (n {cluster: '%s'}) RETURN n", clusterName))

	if error != nil {
		glog.Error("Error getting existing resources for cluster ", clusterName)
		err = error // For return value.
	}
	// Build a map with all the current resources by UID.
	// Build a map of duplicated resources.
	var existingResources = make(map[string]*rg2.Node)
	var duplicatedResources = make(map[string]int)
	for result.Next() {
		record := result.Record()
		if rgNode, ok := record.GetByIndex(0).(*rg2.Node); ok {
			if existingResourceUID, ok := rgNode.Properties["_uid"].(string); ok {
				if _, exists := existingResources[existingResourceUID]; exists {
					dupeCount, dupeExists := duplicatedResources[existingResourceUID]
					if !dupeExists {
						duplicatedResources[existingResourceUID] = 1
					} else {
						duplicatedResources[existingResourceUID] = dupeCount + 1
					}
				} else {
					existingResources[existingResourceUID] = rgNode
				}
			}
		}
	}

	// Delete duplicated records. We have to delete all records with the duplicated UID and recreate.
	if len(duplicatedResources) > 0 {
		glog.Warningf("RedisGraph contains duplicate records for some UIDs in cluster %s. Total uids duplicates: %d",
			clusterName, len(duplicatedResources))
		for dupeUID, dupeCount := range duplicatedResources {
			_, delError := db.Store.Query(db.SanitizeQuery("MATCH (n {_uid:'%s'}) DELETE n", dupeUID))
			if delError != nil {
				glog.Error("Error deleting duplicates for ", dupeUID, delError)
			}
			glog.V(3).Infof("Deleted %d duplicates of UID %s", dupeCount, dupeUID)
			delete(existingResources, dupeUID) // Delete from existing resources.
		}
	}

	// Loop through incoming resources and check if each resource exist and if it needs to be updated.
	var resourcesToAdd = make([]*db.Resource, 0)
	var resourcesToUpdate = make([]*db.Resource, 0)
	for _, newResource := range resources {
		existingResource, exist := existingResources[newResource.UID]

		if !exist {
			// Resource needs to be added.
			resourcesToAdd = append(resourcesToAdd, newResource)
		} else {
			// Resource exists, but we need to check if it needs to be updated.
			newEncodedProperties, encodeError := newResource.EncodeProperties()
			if encodeError != nil {
				// Assume we need to update this resource if we hit an encoding error.
				glog.Warning("Error encoding properties of resource. ", encodeError)
				resourcesToUpdate = append(resourcesToUpdate, newResource)
			} else {
				for key, value := range newEncodedProperties {
					var isInterface bool
					var existingProperty, stringValue string
					_, interfaceTypeTrue := value.([]interface{})
					existingInterface, existingInterfaceTypeTrue := existingResource.Properties[key].([]interface{})
					if interfaceTypeTrue && existingInterfaceTypeTrue {
						isInterface = true
					} else {
						// Need to compare everything other than interfaces as strings
						// because that's what we get from RedisGraph.
						stringValue = valueToString(value)
						existingProperty = valueToString(existingResource.Properties[key])
					}
					if (isInterface && !reflect.DeepEqual(newResource.Properties[key], existingInterface)) ||
						existingProperty != stringValue {
						resourcesToUpdate = append(resourcesToUpdate, newResource)
						break
					}
				}
			}
			// Remove the resource because it has been proccessed.
			// Any resources remaining when we are done will need to be deleted.
			delete(existingResources, newResource.UID)
		}
	}

	// INSERT Resources

	metrics.NodeSyncStart = time.Now()
	insertResponse := db.ChunkedInsert(resourcesToAdd, clusterName)
	stats.TotalAdded = insertResponse.SuccessfulResources // could be 0
	if insertResponse.ConnectionError != nil {
		err = insertResponse.ConnectionError
	} else if len(insertResponse.ResourceErrors) != 0 {
		stats.AddErrors = processSyncErrors(insertResponse.ResourceErrors, "inserted")
	}

	// UPDATE Resources

	updateResponse := db.ChunkedUpdate(resourcesToUpdate)
	stats.TotalUpdated = updateResponse.SuccessfulResources // could be 0
	if updateResponse.ConnectionError != nil {
		err = updateResponse.ConnectionError
	} else if len(updateResponse.ResourceErrors) != 0 {
		stats.UpdateErrors = processSyncErrors(updateResponse.ResourceErrors, "updated")
	}

	// DELETE Resources

	deleteUIDS := make([]string, 0, len(existingResources))
	for _, resource := range existingResources {
		deleteUIDS = append(deleteUIDS, resource.Properties["_uid"].(string))
	}
	deleteResponse := db.ChunkedDelete(deleteUIDS)
	stats.TotalDeleted = deleteResponse.SuccessfulResources // could be 0
	if deleteResponse.ConnectionError != nil {
		err = deleteResponse.ConnectionError
	} else if len(deleteResponse.ResourceErrors) != 0 {
		stats.DeleteErrors = processSyncErrors(deleteResponse.ResourceErrors, "deleted")
	}

	metrics.NodeSyncEnd = time.Now()

	// RE-SYNC Edges

	metrics.EdgeSyncStart = time.Now()

	currEdgesCount := computeIntraEdges(clusterName)
	glog.V(4).Info("Number of intra edges for cluster ", clusterName, " before removing duplicates: ", currEdgesCount)

	currEdges, edgesError := db.Store.Query(fmt.Sprintf("MATCH (s {cluster:'%s'})-[r]->(d {cluster:'%s'}) WHERE (r._interCluster <> true) OR (r._interCluster IS NULL) RETURN s._uid, type(r), d._uid",
		clusterName, clusterName))
	if edgesError != nil {
		glog.Warning("Error getting all existing edges for cluster ", clusterName, edgesError)
		err = edgesError
	}
	var existingEdges = make(map[string]db.Edge)
	var edgesToAdd = make([]db.Edge, 0)

	// Create a map with the existing edges.

	dupCount := 0
	if edgesError == nil { //to avoid panic if there is an error executing query
		for currEdges.Next() {
			e := currEdges.Record()
			key := getEdgeUID(valueToString(e.GetByIndex(0)), valueToString(e.GetByIndex(1)),
				valueToString(e.GetByIndex(2)))
			if _, ok := existingEdges[key]; !ok {
				existingEdges[key] = db.Edge{
					SourceUID: valueToString(e.GetByIndex(0)),
					EdgeType:  valueToString(e.GetByIndex(1)),
					DestUID:   valueToString(e.GetByIndex(2)),
				}
			} else {
				dupCount++
			}
		}
	}

	glog.V(4).Info("Duplicate edge count: ", dupCount)

	//Redisgraph 2.0 supports addition of duplicate edges. Delete duplicate edges, if any, in the cluster
	dupEdgedeleted, delEdgesError := db.Store.Query(fmt.Sprintf("MATCH (s {cluster:'%s'})-[r]->(d {cluster:'%s'}) WHERE (r._interCluster <> true) OR (r._interCluster IS NULL) WITH s as source, d as dest, TYPE(r) as edge, COLLECT (r) AS edges WHERE size(edges) >1 UNWIND edges[1..] AS dupedges DELETE dupedges", clusterName, clusterName))
	if delEdgesError != nil {
		glog.Warning("Error deleting duplicate edges for cluster ", clusterName, delEdgesError)
		err = delEdgesError
	} else {
		glog.V(4).Info("For cluster, ", clusterName, ": Deleted duplicate edges: ", dupEdgedeleted.RelationshipsDeleted())
	}

	currEdgesCount = computeIntraEdges(clusterName)
	glog.V(4).Info("Number of intra edges for cluster ", clusterName, " after removing duplicates: ", currEdgesCount)

	existingEdgesMapLength := len(existingEdges)
	glog.V(4).Info("Existing edges map length: ", len(existingEdges))

	var verifyEdges = make(map[string]bool)

	//Loop through incoming new edges and decide if each edge needs to be added.
	for _, e := range edges {
		verifyEdges[getEdgeUID(e.SourceUID, e.EdgeType, e.DestUID)] = true
		if _, exists := existingEdges[getEdgeUID(e.SourceUID, e.EdgeType, e.DestUID)]; exists {
			delete(existingEdges, getEdgeUID(e.SourceUID, e.EdgeType, e.DestUID))
		} else {
			edgesToAdd = append(edgesToAdd, e)
		}
	}
	if len(verifyEdges) != len(edges) {
		glog.Error("There are duplicate edges in the payload from cluster: ", clusterName)
	}

	// Compute edges to delete.
	// These are the remaining objects in existingEdges after processing all the incoming new edges.
	var edgesToDelete = make([]db.Edge, 0)
	for _, e := range existingEdges {
		edgesToDelete = append(edgesToDelete, e)
	}

	expectedEdgesAfterProcessing := existingEdgesMapLength + len(edgesToAdd) - len(edgesToDelete)
	if expectedEdgesAfterProcessing != len(edges) {
		glog.Warningf("For cluster %s expectedEdgesAfterProcessing [%d] doesn't match received len(edges) [%d]",
			clusterName, expectedEdgesAfterProcessing, len(edges))
	}
	// INSERT Edges
	glog.V(4).Info("Resync for cluster ", clusterName, ": Number of edges to insert: ", len(edgesToAdd))
	insertEdgeResponse := db.ChunkedInsertEdge(edgesToAdd, clusterName)
	stats.TotalEdgesAdded = insertEdgeResponse.SuccessfulResources // could be 0
	if insertEdgeResponse.ConnectionError != nil {
		err = insertEdgeResponse.ConnectionError
	} else if len(insertEdgeResponse.ResourceErrors) != 0 {
		stats.AddEdgeErrors = processSyncErrors(insertEdgeResponse.ResourceErrors, "inserted by edge")
	}

	if len(edgesToAdd) != insertEdgeResponse.EdgesAdded {
		glog.V(4).Info("Edges to add len: ", len(edgesToAdd))
		glog.V(4).Info("Edge add errors: ", len(insertEdgeResponse.ResourceErrors))
		glog.V(4).Info("Edge add errors: ", insertEdgeResponse.ResourceErrors)
		currEdgesCount = computeIntraEdges(clusterName)
		glog.V(4).Infof("Number of intra edges for cluster %s after adding edges: %d",
			clusterName, currEdgesCount)
		glog.V(4).Info("currEdgesCount: ", currEdgesCount, " incoming edges: ", len(edges))
		glog.V(4).Infof("Added edge count %d didn't match expected number: %d",
			insertEdgeResponse.EdgesAdded, len(edgesToAdd))
	}

	// DELETE Edges
	glog.V(4).Info("Resync for cluster ", clusterName, ": Number of edges to delete: ", len(edgesToDelete))
	deleteEdgeResponse := db.ChunkedDeleteEdge(edgesToDelete, clusterName)
	stats.TotalEdgesDeleted = deleteEdgeResponse.SuccessfulResources // could be 0
	if deleteEdgeResponse.ConnectionError != nil {
		err = deleteEdgeResponse.ConnectionError
	} else if len(deleteEdgeResponse.ResourceErrors) != 0 {
		stats.DeleteEdgeErrors = processSyncErrors(deleteEdgeResponse.ResourceErrors, "removed by edge")
	}

	if len(edgesToDelete) != deleteEdgeResponse.EdgesDeleted {
		glog.V(4).Info("Edges to delete: len", len(edgesToDelete))
		glog.V(4).Info("Edge delete errors: ", len(deleteEdgeResponse.ResourceErrors))
		glog.V(4).Info("Edge delete errors: ", deleteEdgeResponse.ResourceErrors)
		currEdgesCount = computeIntraEdges(clusterName)
		glog.V(4).Info("Number of intra edges for cluster ", clusterName, " after deleting edges: ", currEdgesCount)
		glog.V(4).Info("currEdgesCount: ", currEdgesCount, " incoming edges: ", len(edges))
		glog.V(4).Infof("Deleted edge count %d didn't match expected number: %d",
			deleteEdgeResponse.EdgesDeleted, len(edgesToDelete))
	}

	// There's no need to UPDATE edges because edges don't have properties yet.

	metrics.EdgeSyncEnd = time.Now()
	glog.V(4).Infof("resyncCluster complete. Done updating resources for cluster %s, preparing response", clusterName)

	return stats, err
}

func valueToString(value interface{}) string {
	var stringValue string
	switch typedVal := value.(type) {
	case int64:
		stringValue = strconv.FormatInt(typedVal, 10)
	case int:
		stringValue = strconv.Itoa(typedVal)
	default:
		if _, ok := typedVal.(string); ok {
			stringValue = typedVal.(string)
		} else {
			glog.Warning("Unable to parse string value from interface{} :  ", typedVal)
		}
	}
	return stringValue
}
