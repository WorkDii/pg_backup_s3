package main

import "time"

// Object is the subset of an S3 listing entry that retention cares about.
type Object struct {
	Key      string
	Modified time.Time
}

// selectExpired returns the keys older than cutoff, always excluding keep.
//
// keep is the object written by the run that triggered this prune. Excluding
// it explicitly means a clock skew between this container and the storage
// provider can never delete the backup that was just taken.
func selectExpired(objs []Object, cutoff time.Time, keep string) []string {
	var expired []string
	for _, o := range objs {
		if o.Key == keep {
			continue
		}
		if o.Modified.Before(cutoff) {
			expired = append(expired, o.Key)
		}
	}
	return expired
}

// chunk splits keys into batches of at most size. S3 DeleteObjects accepts a
// maximum of 1000 keys per request.
func chunk(keys []string, size int) [][]string {
	var batches [][]string
	for len(keys) > 0 {
		n := size
		if len(keys) < n {
			n = len(keys)
		}
		batches = append(batches, keys[:n])
		keys = keys[n:]
	}
	return batches
}
