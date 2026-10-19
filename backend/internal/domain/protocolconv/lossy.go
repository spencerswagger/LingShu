package protocolconv

type LossyReport struct {
	Dropped []string `json:"dropped,omitempty"`
	Notes   []string `json:"notes,omitempty"`
}

func (r *LossyReport) Add(field string) {
	if r == nil || field == "" {
		return
	}
	r.Dropped = append(r.Dropped, field)
}

func (r *LossyReport) Note(note string) {
	if r == nil || note == "" {
		return
	}
	r.Notes = append(r.Notes, note)
}

func (r *LossyReport) Merge(o *LossyReport) {
	if r == nil || o == nil {
		return
	}
	r.Dropped = append(r.Dropped, o.Dropped...)
	r.Notes = append(r.Notes, o.Notes...)
}

func (r *LossyReport) IsEmpty() bool {
	if r == nil {
		return true
	}
	return len(r.Dropped) == 0 && len(r.Notes) == 0
}
