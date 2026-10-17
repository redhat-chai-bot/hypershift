package main

import (
	"reflect"
	"strings"

	"k8s.io/gengo/types"
)

func fieldName(member types.Member) string {
	name, _, _ := strings.Cut(reflect.StructTag(member.Tags).Get("json"), ",")
	if name != "" {
		return name
	}
	return member.Name
}
