package api

import (
	"errors"
	"net/http"

	"github.com/gamerx27/x27-linux-homelab/dashboard/internal/storage"
)

// mountBody is a mount from the Storage page, with the SMB password: write-only, it's
// never sent back.
type mountBody struct {
	storage.Mount
	Password string `json:"password"`
}

var errClusterShare = errors.New("this is a cluster share: change or remove it on the main node, under Cluster → Shared storage")

// storageRoutes: NFS and SMB shares mounted on the node.
func (l *Local) storageRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /storage/mounts", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"mounts": storage.List()})
	})
	mux.HandleFunc("GET /storage/exports", func(w http.ResponseWriter, r *http.Request) {
		list, err := storage.Exports(r.URL.Query().Get("server"))
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"exports": list})
	})
	mux.HandleFunc("POST /storage/test-mount", func(w http.ResponseWriter, r *http.Request) {
		var body mountBody
		if err := readJSON(r, &body); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		out, err := storage.Test(body.Mount, body.Password)
		result(w, r, "test mount "+body.Server, out, err)
	})
	mux.HandleFunc("POST /storage/mounts", func(w http.ResponseWriter, r *http.Request) {
		var body mountBody
		if err := readJSON(r, &body); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		result(w, r, "add mount "+body.Name, "", storage.Save(body.Mount, body.Password, true))
	})
	mux.HandleFunc("POST /storage/mounts/{name}", func(w http.ResponseWriter, r *http.Request) {
		var body mountBody
		if err := readJSON(r, &body); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		body.Name = r.PathValue("name")
		if storage.IsCluster(body.Name) && !body.Cluster {
			writeErr(w, http.StatusConflict, errClusterShare)
			return
		}
		result(w, r, "change mount "+body.Name, "", storage.Save(body.Mount, body.Password, false))
	})
	mux.HandleFunc("POST /storage/mounts/{name}/{action}", func(w http.ResponseWriter, r *http.Request) {
		name, action := r.PathValue("name"), r.PathValue("action")
		var body struct{ Cluster bool }
		readJSON(r, &body)
		if action == "remove" && storage.IsCluster(name) && !body.Cluster {
			writeErr(w, http.StatusConflict, errClusterShare)
			return
		}
		result(w, r, "storage "+action+" "+name, "", storage.Action(name, action))
	})
}
