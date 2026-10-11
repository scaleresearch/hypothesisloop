#!/usr/bin/env bash
# The clusters on the platform's own machines, as OWNED_COMPUTE_CLUSTERS in the repository's .env
# lists them (.env.example): entries separated by spaces or commas, each
# <name>:<provider>[:<field>=<value>]..., where provider is how the cluster runs and decides
# which fields it takes. A customer's cluster is registered through the product instead.
LIB_CLUSTERS_REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"

# lib_owned_clusters: one line per entry, its name, its provider and then its fields, each
# checked against what the provider takes. Fails on an entry that is not one.
lib_owned_clusters() {
  local list entry name provider field i
  local -a parts
  [[ -f "${LIB_CLUSTERS_REPO}/.env" ]] || return 0
  list="$(set +u; . "${LIB_CLUSTERS_REPO}/.env"; printf '%s' "${OWNED_COMPUTE_CLUSTERS:-}")"
  for entry in ${list//,/ }; do
    IFS=: read -r -a parts <<<"${entry}"
    name="${parts[0]}" provider="${parts[1]:-}"
    case "${provider}" in
      k8s | bare-metal) ;;
      *)
        echo "OWNED_COMPUTE_CLUSTERS: '${entry}' names provider '${provider}', and a cluster of the platform's own runs on k8s or bare-metal" >&2
        return 1
        ;;
    esac
    for ((i = 2; i < ${#parts[@]}; i++)); do
      field="${parts[i]}"
      case "${provider}:${field%%=*}" in
        *:silence_ceiling_seconds | k8s:context | k8s:node) ;;
        *)
          echo "OWNED_COMPUTE_CLUSTERS: '${entry}' sets '${field%%=*}', which a ${provider} cluster does not take (.env.example lists the fields)" >&2
          return 1
          ;;
      esac
      if [[ "${field}" != *=?* ]]; then
        echo "OWNED_COMPUTE_CLUSTERS: '${entry}' sets '${field}' with no value" >&2
        return 1
      fi
    done
    echo "${name} ${provider} ${parts[*]:2}"
  done
}

# lib_cluster_field <field> <line of lib_owned_clusters>: the field's value, empty when unset.
lib_cluster_field() {
  local want="$1" field
  for field in $2; do
    if [[ "${field%%=*}" == "${want}" ]]; then
      printf '%s' "${field#*=}"
    fi
  done
}

# lib_owned_cluster_at <context>: the line of the k8s cluster this machine reaches under the kube
# context, empty when no entry names it.
lib_owned_cluster_at() {
  local clusters line
  clusters="$(lib_owned_clusters)" || return 1
  while read -r line; do
    if [[ -n "${line}" && "$(lib_cluster_field context "${line}")" == "$1" ]]; then
      printf '%s' "${line}"
    fi
  done <<<"${clusters}"
}
