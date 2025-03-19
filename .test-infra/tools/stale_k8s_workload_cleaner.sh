#!/usr/bin/env bash
#
#    Licensed to the Apache Software Foundation (ASF) under one or more
#    contributor license agreements.  See the NOTICE file distributed with
#    this work for additional information regarding copyright ownership.
#    The ASF licenses this file to You under the Apache License, Version 2.0
#    (the "License"); you may not use this file except in compliance with
#    the License.  You may obtain a copy of the License at
#
#       http://www.apache.org/licenses/LICENSE-2.0
#
#    Unless required by applicable law or agreed to in writing, software
#    distributed under the License is distributed on an "AS IS" BASIS,
#    WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
#    See the License for the specific language governing permissions and
#    limitations under the License.
#
#    Deletes stale and old BQ datasets that are left after tests.
#

set -euo pipefail

# Clean up the stale kubernetes workload of given cluster

PROJECT=apache-beam-testing
LOCATION=us-central1-a
CLUSTER=io-datastores

function should_teardown() {
  if [[ $1 =~ ^([0-9]+)([a-z]) ]]; then
    local time_scale=${BASH_REMATCH[1]}
    local time_unit=${BASH_REMATCH[2]}
    # cutoff = 8 h
    if [ $time_unit == y ] || [ $time_unit == d ]; then
      return 0
    elif [ $time_unit == h ] && [ $time_scale -ge 8 ]; then
      return 0
    fi
  fi
  return 1
}

function clean_namespace() {
  local ns=$1
  echo "Cleaning remaining resources in namespace $ns..."

  # Get all namespaced resource types (including CRDs)
  local resource_types
  resource_types=$(kubectl api-resources --verbs=list --namespaced -o name)

  # For each resource type, list and delete all instances in the namespace.
  for resource in $resource_types; do
    local items
    items=$(kubectl get "$resource" -n "$ns" -o name 2>/dev/null || true)
    if [ -n "$items" ]; then
      echo "Deleting resources of type $resource in namespace $ns:"
      echo "$items"
      echo "$items" | xargs -r kubectl delete -n "$ns"
    fi
  done

  # Wait until no resource (including custom ones) remains.
  local timeout=300
  while true; do
    local remaining=""
    for resource in $resource_types; do
      local items
      items=$(kubectl get "$resource" -n "$ns" --ignore-not-found --no-headers 2>/dev/null || true)
      if [ -n "$items" ]; then
         remaining="${remaining}${items}"
      fi
    done
    if [ -z "$remaining" ]; then
      echo "All resources in namespace $ns have been deleted."
      break
    fi
    echo "Waiting for resources to be deleted in namespace $ns..."
    sleep 5
    timeout=$((timeout-5))
    if [ $timeout -le 0 ]; then
      echo "Timeout reached while waiting for resources to be deleted in namespace $ns."
      break
    fi
  done
}

gcloud container clusters get-credentials io-datastores --zone us-central1-a --project apache-beam-testing

while read NAME STATUS AGE; do
  # Regex has temporary workaround to avoid trying to delete beam-performancetests-singlestoreio-* to avoid getting stuck in a terminal state
  # See https://github.com/apache/beam/pull/33545 for context.
  # This may be safe to remove if https://cloud.google.com/knowledge/kb/deleted-namespace-remains-in-terminating-status-000004867 has been resolved, just try it before checking in :)
  if [[ $NAME =~ ^beam-.+(test|-it) ]] && should_teardown $AGE; then
    echo "Processing namespace $NAME with age $AGE..."
    # First, clean the namespace by deleting any remaining resources.
    clean_namespace "$NAME"
    # Once the namespace is clear, delete it.
    echo "Deleting namespace $NAME..."
    kubectl delete namespace "$NAME"
  fi
done < <( kubectl get namespaces --context=gke_${PROJECT}_${LOCATION}_${CLUSTER} )
