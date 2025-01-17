package retag

import (
	"fmt"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"unsafe"
)

// TODO(yar): Write implementation notes for TagMaker.

// A TagMaker interface is used by the Convert function to generate tags for structures.
// A type that implements TagMaker should be comparable.
type TagMaker interface {
	// MakeTag makes tag for the field the fieldIndex in the structureType.
	// Result should depends on constant parameters of creation of the TagMaker and parameters
	// passed to the MakeTag. The MakeTag should not produce side effects (like a pure function).
	MakeTag(structureType reflect.Type, fieldIndex int) reflect.StructTag
}

// Convert converts the given interface p, to a runtime-generated type.
// The type is generated on base of source type by the next rules:
//   - Analogous type with custom tags is generated for structures.
//     The tag for every field is generated by the maker;
//   - Type is replaced with a generated one if it has field, element or key of type
//     which should be replaced with its own analogue or if it is structure.
//   - A type of private fields of structures is not modified.
//
// Convert panics if argument p has a type different from a pointer to structure.
// The maker's underlying type should be comparable. In different case panic occurs.
//
// Convert panics if the maker attempts to change a field tag of a structure with unexported fields
// because reflect package doesn't support creation of a structure type with private fields.
//
// Convert puts generated types in a cache by a key (source type + maker) to speed up
// handling of types. See notes in description of TagMaker interface to avoid
// the tricky situation with the cache.
//
// Convert doesn't support cyclic references because reflect package doesn't support generation of
// types with cyclic references. Passing cyclic structures to Convert will result in an infinite
// recursion.
//
// Convert doesn't support any interfaces, functions, chan and unsafe pointers.
// Interfaces is not supported because they requires memory-copy operations in most cases.
// Passing structures that contains unsupported types to Convert will result in a panic.
//
// Convert doesn't reconstruct methods for a structure type until go1.9
// because it is not supported by reflect package.
// Convert can raise a panic since go1.9 if a structure derivative type has too much methods (more than 32).
//
// BUG(yar): Convert panics on structure with a final zero-size field in go1.7.
// It is fixed in go1.8 (see github.com/golang/go/issues/18016).
func Convert(p interface{}, maker TagMaker) interface{} {
	return convert(p, maker, false)
}

// ConvertAny is basically the same as Convert except it doesn't panic in case if struct field has empty interface type,
// it's just left unchanged
func ConvertAny(p interface{}, maker TagMaker) interface{} {
	return convert(p, maker, true)
}

func convert(p interface{}, maker TagMaker, any bool) interface{} {
	strPtrVal := reflect.ValueOf(p)
	// TODO(yar): check type (pointer to the structure)
	res := getType(strPtrVal.Type().Elem(), maker, any, map[string]bool{})
	newPtrVal := reflect.NewAt(res.t, unsafe.Pointer(strPtrVal.Pointer()))
	return newPtrVal.Interface()
}

type cacheKey struct {
	reflect.Type
	TagMaker
}

type result struct {
	t        reflect.Type
	changed  bool
	hasIface bool
	finishedProcessing bool
}

var cache = struct {
	sync.RWMutex
	m map[cacheKey]result
}{
	m: make(map[cacheKey]result),
}

func getType(structType reflect.Type, maker TagMaker, any bool, seen map[string]bool) result {
	// TODO(yar): Improve synchronization for cases when one analogue
	// is produced concurently by different goroutines in the same time
	key := cacheKey{structType, maker}
	cache.RLock()
	res, ok := cache.m[key]
	cache.RUnlock()
	if !ok || (res.hasIface && !any) {
		res = makeType(structType, maker, any, seen)
		cache.Lock()
		cache.m[key] = res
		cache.Unlock()
	}
	return res
}

func makeType(t reflect.Type, maker TagMaker, any bool, seen map[string]bool) result {
	switch t.Kind() {
	case reflect.Struct:
		key:=fmt.Sprintf("%s.%s", t.PkgPath(), t.Name())
		if seen[key] {
			return result{t: t, changed: false}
		}
		seen[key] = true
		return makeStructType(t, maker, any, seen)
	case reflect.Ptr:
		res := getType(t.Elem(), maker, any,seen)
		if !res.changed {
			return result{t: t, changed: false}
		}
		return result{t: reflect.PtrTo(res.t), changed: true}
	case reflect.Array:
		res := getType(t.Elem(), maker, any,seen)
		if !res.changed {
			return result{t: t, changed: false}
		}
		return result{t: reflect.ArrayOf(t.Len(), res.t), changed: true}
	case reflect.Slice:
		res := getType(t.Elem(), maker, any,seen)
		if !res.changed {
			return result{t: t, changed: false}
		}
		return result{t: reflect.SliceOf(res.t), changed: true}
	case reflect.Map:
		resKey := getType(t.Key(), maker, any,seen)
		resElem := getType(t.Elem(), maker, any,seen)
		if !resKey.changed && !resElem.changed {
			return result{t: t, changed: false}
		}
		return result{t: reflect.MapOf(resKey.t, resElem.t), changed: true}
	case reflect.Interface:
		if any {
			return result{t: t, changed: false, hasIface: true}
		}
		fallthrough
	case
		reflect.Chan,
		reflect.Func,
		reflect.UnsafePointer:
		panic("tags.Map: Unsupported type: " + t.Kind().String())
	default:
		// don't modify type in another case
		return result{t: t, changed: false}
	}
}

func makeStructType(structType reflect.Type, maker TagMaker, any bool, seen map[string]bool) result {
	if structType.NumField() == 0 {
		return result{t: structType, changed: false}
	}
	changed := false
	hasPrivate := false
	hasIface := false
	fields := make([]reflect.StructField, 0, structType.NumField())
	for i := 0; i < structType.NumField(); i++ {
		strField := structType.Field(i)
		if isExported(strField.Name) {
			oldType := strField.Type
			new := getType(oldType, maker, any,seen)
			strField.Type = new.t
			if oldType != new.t {
				changed = true
			}
			if new.hasIface {
				hasIface = true
			}
			oldTag := strField.Tag
			newTag := maker.MakeTag(structType, i)
			strField.Tag = newTag
			if oldTag != newTag {
				changed = true
			}
		} else {
			hasPrivate = true
			if !structTypeConstructorBugWasFixed {
				// reflect.StructOf works with private fields and anonymous fields incorrect.
				// see issue https://github.com/golang/go/issues/17766
				strField.PkgPath = ""
				strField.Name = ""
			}
		}
		fields = append(fields, strField)
	}
	if !changed {
		return result{t: structType, changed: false, hasIface: hasIface}
	} else if hasPrivate {
		panic(fmt.Sprintf("unable to change tags for type %s, because it contains unexported fields", structType))
	}
	newType := reflect.StructOf(fields)
	compareStructTypes(structType, newType)
	return result{t: newType, changed: true, hasIface: hasIface}
}

func isExported(name string) bool {
	b := name[0]
	return !('a' <= b && b <= 'z') && b != '_'
}

func compareStructTypes(source, result reflect.Type) {
	if source.Size() != result.Size() {
		// TODO: debug
		// fmt.Println(newType.Size(), newType)
		// for i := 0; i < newType.NumField(); i++ {
		// 	fmt.Println(newType.Field(i))
		// }
		// fmt.Println(structType.Size(), structType)
		// for i := 0; i < structType.NumField(); i++ {
		// 	fmt.Println(structType.Field(i))
		// }
		panic("tags.Map: Unexpected case - type has a size different from size of original type")
	}
}

var structTypeConstructorBugWasFixed bool

func init() {
	switch {
	case strings.HasPrefix(runtime.Version(), "go1.7"):
		// there is bug in reflect.StructOf
	default:
		structTypeConstructorBugWasFixed = true
	}
}
