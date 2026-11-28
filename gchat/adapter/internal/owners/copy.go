package owners

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"time"
)

// parseCopy holds root's copy to its one shape: exactly "version", the number
// 1, and "owners", a map of at most 50 Chat user ids to records with exactly
// since, by "page-code" and claim. Anything else is not trusted.
func parseCopy(data []byte) (map[string]Record, string) {
	top, err := object(data)
	if err != nil {
		return nil, "json"
	}
	if len(top) != 2 || top["version"] == nil || top["owners"] == nil {
		return nil, "keys"
	}
	if string(top["version"]) != "1" {
		return nil, "version"
	}
	entries, err := object(top["owners"])
	if err != nil {
		return nil, "owners"
	}
	if len(entries) > MaxOwners {
		return nil, "too_many"
	}
	owners := make(map[string]Record, len(entries))
	for id, raw := range entries {
		if !ChatUserID.MatchString(id) {
			return nil, "owner_id"
		}
		fields, err := object(raw)
		if err != nil || len(fields) != 3 {
			return nil, "record"
		}
		var r Record
		for name, target := range map[string]*string{"since": &r.Since, "by": &r.By, "claim": &r.Claim} {
			if fields[name] == nil || json.Unmarshal(fields[name], target) != nil {
				return nil, "record"
			}
		}
		if _, err := time.Parse(time.RFC3339, r.Since); err != nil || !since.MatchString(r.Since) || r.By != "page-code" || !claimID.MatchString(r.Claim) {
			return nil, "record"
		}
		owners[id] = r
	}
	return owners, ""
}

var errShape = errors.New("owners: not one JSON object with distinct keys")

// object reads exactly one JSON object and refuses a repeated key, a value of
// null and anything after the object.
func object(data []byte) (map[string]json.RawMessage, error) {
	d := json.NewDecoder(bytes.NewReader(data))
	if tok, err := d.Token(); err != nil || tok != json.Delim('{') {
		return nil, errShape
	}
	out := map[string]json.RawMessage{}
	for d.More() {
		tok, err := d.Token()
		key, ok := tok.(string)
		if err != nil || !ok {
			return nil, errShape
		}
		var raw json.RawMessage
		if _, seen := out[key]; seen || d.Decode(&raw) != nil || string(raw) == "null" {
			return nil, errShape
		}
		out[key] = raw
	}
	if tok, err := d.Token(); err != nil || tok != json.Delim('}') {
		return nil, errShape
	}
	if _, err := d.Token(); err != io.EOF {
		return nil, errShape
	}
	return out, nil
}
