// Package memsize estimates how many bytes a Go value holds in
// memory: its own size plus everything it reaches through pointers,
// slices, maps, strings and interfaces. Large slices and maps are
// sampled (a few dozen elements, scaled up to the full length), so
// an estimate over millions of entries costs milliseconds, not a
// walk of every entry. It is an estimate for the admin page's
// memory breakdown, never an exact figure: shared pointers count
// once per holder, and map overhead is a fixed allowance.
package memsize

import (
	"reflect"
	"time"
)

// sample is how many elements of a slice or map are measured; the
// rest are taken to look like them.
const sample = 32

// maxDepth stops the walk on deep or cyclic structures.
const maxDepth = 12

// mapSlotOverhead is the bytes a map spends per entry beyond its key
// and value: control bytes and the free slots its load factor keeps.
const mapSlotOverhead = 1.4

var locationType = reflect.TypeFor[time.Location]()

// Of is the estimated bytes v holds, its own size included.
func Of(v any) int64 {
	if v == nil {
		return 0
	}
	rv := reflect.ValueOf(v)
	return int64(rv.Type().Size()) + indirect(rv, 0)
}

// indirect is the bytes v reaches beyond its own inline size.
func indirect(v reflect.Value, depth int) int64 {
	if depth > maxDepth || !v.IsValid() {
		return 0
	}
	switch v.Kind() {
	case reflect.String:
		return int64(v.Len())
	case reflect.Pointer:
		if v.IsNil() {
			return 0
		}
		e := v.Elem()
		if e.Type() == locationType {
			return 0 // shared by every time.Time; never the holder's own
		}
		return int64(e.Type().Size()) + indirect(e, depth+1)
	case reflect.Interface:
		if v.IsNil() {
			return 0
		}
		e := v.Elem()
		return int64(e.Type().Size()) + indirect(e, depth+1)
	case reflect.Slice:
		if v.IsNil() {
			return 0
		}
		own := int64(v.Cap()) * int64(v.Type().Elem().Size())
		return own + sampled(v.Len(), func(i int) int64 { return indirect(v.Index(i), depth+1) }, !hasRefs(v.Type().Elem()))
	case reflect.Array:
		return sampled(v.Len(), func(i int) int64 { return indirect(v.Index(i), depth+1) }, !hasRefs(v.Type().Elem()))
	case reflect.Map:
		if v.IsNil() {
			return 0
		}
		n := v.Len()
		kv := float64(v.Type().Key().Size() + v.Type().Elem().Size())
		own := int64(float64(n) * kv * mapSlotOverhead)
		if n == 0 || (!hasRefs(v.Type().Key()) && !hasRefs(v.Type().Elem())) {
			return own
		}
		var sum int64
		seen := 0
		it := v.MapRange()
		for seen < sample && it.Next() {
			sum += indirect(it.Key(), depth+1) + indirect(it.Value(), depth+1)
			seen++
		}
		return own + sum*int64(n)/int64(seen)
	case reflect.Struct:
		var sum int64
		for _, field := range v.Fields() {
			sum += indirect(field, depth+1)
		}
		return sum
	}
	return 0
}

// sampled sums f over n elements, measuring at most sample of them
// spread evenly and scaling the sum to n. flat skips the walk for
// elements that reach nothing.
func sampled(n int, f func(int) int64, flat bool) int64 {
	if n == 0 || flat {
		return 0
	}
	if n <= sample {
		var sum int64
		for i := range n {
			sum += f(i)
		}
		return sum
	}
	var sum int64
	for k := range sample {
		sum += f(k * n / sample)
	}
	return sum * int64(n) / sample
}

// hasRefs reports whether a value of type t can reach memory beyond
// itself.
func hasRefs(t reflect.Type) bool {
	switch t.Kind() {
	case reflect.String, reflect.Pointer, reflect.Interface, reflect.Slice, reflect.Map:
		return true
	case reflect.Array:
		return t.Len() > 0 && hasRefs(t.Elem())
	case reflect.Struct:
		for field := range t.Fields() {
			if hasRefs(field.Type) {
				return true
			}
		}
	}
	return false
}
