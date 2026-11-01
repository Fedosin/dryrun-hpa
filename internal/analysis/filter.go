/*
Copyright The dryrun-hpa Authors.

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

package analysis

import (
	"fmt"
	"net/url"
	"strconv"
	"time"

	"github.com/fedosin/dryrun-hpa/internal/store"
)

// ParseTime accepts RFC 3339 timestamps or a duration meaning "that long before now".
func ParseTime(s string, now time.Time) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	if d, err := time.ParseDuration(s); err == nil {
		return now.Add(-d), nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid time %q: use RFC 3339 or a duration such as 2h", s)
	}
	return t, nil
}

// FilterFromQuery parses the query parameters of the export API.
func FilterFromQuery(q url.Values, now time.Time) (store.Filter, error) {
	f := store.Filter{
		Namespace:    q.Get("namespace"),
		Name:         q.Get("name"),
		SpecHash:     q.Get("specHash"),
		OnlyRescales: q.Get("onlyRescales") == "true",
	}
	var err error
	if f.Since, err = ParseTime(q.Get("since"), now); err != nil {
		return f, err
	}
	if f.Until, err = ParseTime(q.Get("until"), now); err != nil {
		return f, err
	}
	if v := q.Get("afterId"); v != "" {
		if f.AfterID, err = strconv.ParseInt(v, 10, 64); err != nil {
			return f, fmt.Errorf("invalid afterId %q", v)
		}
	}
	if v := q.Get("limit"); v != "" {
		if f.Limit, err = strconv.Atoi(v); err != nil || f.Limit < 0 {
			return f, fmt.Errorf("invalid limit %q", v)
		}
	}
	return f, nil
}

// Query encodes a filter as export API query parameters.
func Query(f store.Filter) url.Values {
	q := url.Values{}
	set := func(k, v string) {
		if v != "" {
			q.Set(k, v)
		}
	}
	set("namespace", f.Namespace)
	set("name", f.Name)
	set("specHash", f.SpecHash)
	if !f.Since.IsZero() {
		q.Set("since", f.Since.Format(time.RFC3339))
	}
	if !f.Until.IsZero() {
		q.Set("until", f.Until.Format(time.RFC3339))
	}
	if f.AfterID > 0 {
		q.Set("afterId", strconv.FormatInt(f.AfterID, 10))
	}
	if f.Limit > 0 {
		q.Set("limit", strconv.Itoa(f.Limit))
	}
	if f.OnlyRescales {
		q.Set("onlyRescales", "true")
	}
	return q
}
