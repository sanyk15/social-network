package httpapi

import (
	"log"
	"net/http"
)

// TestWrite appends one row to write_load. It is registered only when
// ENABLE_TEST_API=true and is used by the replication write-load scenario.
func (h *Handler) TestWrite(w http.ResponseWriter, r *http.Request) {
	id, err := h.store.InsertWriteLoad(r.Context(), "k6")
	if err != nil {
		log.Printf("test write: %v", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]int64{"id": id})
}

// TestWriteCount reports how many rows are visible on the current master.
func (h *Handler) TestWriteCount(w http.ResponseWriter, r *http.Request) {
	count, err := h.store.CountWriteLoad(r.Context())
	if err != nil {
		log.Printf("test write count: %v", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]int64{"count": count})
}

// TestReplication returns a snapshot of replication state and read routing.
func (h *Handler) TestReplication(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, h.store.ReplicationStatus(r.Context()))
}
