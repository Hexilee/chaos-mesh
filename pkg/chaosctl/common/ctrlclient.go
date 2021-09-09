// Copyright 2021 Chaos Mesh Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package common

import (
	"context"
	"fmt"
	"reflect"
	"strconv"
	"strings"

	"github.com/gertd/go-pluralize"
	prmt "github.com/gitchander/permutation"
	comb "github.com/gitchander/permutation/combination"
	"github.com/hasura/go-graphql-client"
	"github.com/iancoleman/strcase"
)

const NamespaceKey = "namespace"
const NamespaceType = "Namespace"

type CtrlClient struct {
	ctx context.Context

	Client             *graphql.Client
	SubscriptionClient *graphql.SubscriptionClient
	Schema             *Schema
}

type AutoCompleteContext struct {
	maxRecurLevel  int
	visitedTypes   map[string]bool
	completeLeaves bool
	query          []string
}

func NewAutoCompleteContext(namespace string, level int, completeLeaves bool) *AutoCompleteContext {
	return &AutoCompleteContext{
		maxRecurLevel:  level,
		visitedTypes:   make(map[string]bool),
		completeLeaves: completeLeaves,
		query:          []string{NamespaceKey, namespace},
	}
}

func (ctx *AutoCompleteContext) IsComplete() bool {
	return ctx.maxRecurLevel == 0
}

func (ctx *AutoCompleteContext) Visited(name string) bool {
	return ctx.visitedTypes[name]
}

func (ctx *AutoCompleteContext) Next(typename, fieldName, arg string) *AutoCompleteContext {
	types := map[string]bool{
		typename: true,
	}

	query := make([]string, 0)

	for name := range ctx.visitedTypes {
		types[name] = true
	}

	for _, seg := range ctx.query {
		query = append(query, seg)
	}

	query = append(query, fieldName)
	if arg != "" {
		query = append(query, arg)
	}

	return &AutoCompleteContext{
		maxRecurLevel:  ctx.maxRecurLevel - 1,
		visitedTypes:   types,
		completeLeaves: ctx.completeLeaves,
		query:          query,
	}
}

func NewCtrlClient(ctx context.Context, url string) (*CtrlClient, error) {
	client := &CtrlClient{
		ctx:                ctx,
		Client:             graphql.NewClient(url, nil),
		SubscriptionClient: graphql.NewSubscriptionClient(url),
	}

	schemaQuery := new(struct {
		Schema RawSchema `graphql:"__schema"`
	})

	err := client.Client.Query(client.ctx, schemaQuery, nil)
	if err != nil {
		return nil, err
	}

	client.Schema = NewSchema(&schemaQuery.Schema)
	return client, nil
}

func (c *CtrlClient) GetQueryType() (*Type, error) {
	return c.Schema.MustGetType(string(c.Schema.QueryType.Name))
}

// list tail arguments, expected queryStr: ["prefix1", "prefix2", "resource", "<some value> can be empty"]
func (c *CtrlClient) ListArguments(queryStr []string, argumentName string) ([]string, error) {
	queryType, err := c.GetQueryType()
	if err != nil {
		return nil, err
	}

	listQuery := queryStr[:len(queryStr)-1]
	helper := pluralize.NewClient()
	listQuery[len(listQuery)-1] = helper.Plural(listQuery[len(listQuery)-1])
	listQuery = append(listQuery, argumentName)
	query, err := c.Schema.ParseQuery(listQuery, queryType)
	if err != nil {
		return nil, err
	}

	superQuery := NewQuery("query", queryType, nil)
	superQuery.Fields["namespace"] = query
	variables := NewVariables()

	queryStruct, err := c.Schema.Reflect(superQuery, variables)
	if err != nil {
		return nil, err
	}

	queryValue := reflect.New(queryStruct.Elem()).Interface()
	err = c.Client.Query(c.ctx, queryValue, variables.GenMap())
	if err != nil {
		return nil, err
	}

	arguments, err := listArguments(queryValue, query, queryStr[len(queryStr)-1])
	if err != nil {
		return nil, err
	}

	return arguments, err
}

func listArguments(object interface{}, resource *Query, startWith string) ([]string, error) {
	value := reflect.ValueOf(object)
	switch value.Kind() {
	case reflect.Ptr:
		return listArguments(value.Elem().Interface(), resource, startWith)
	case reflect.Struct:
		field := value.FieldByName(strcase.ToCamel(resource.Name))
		if field == *new(reflect.Value) {
			return nil, fmt.Errorf("cannot find field %s in object: %#v", resource.Name, object)
		}
		for _, f := range resource.Fields {
			return listArguments(field.Interface(), f, startWith)
		}
		return listArguments(field.Interface(), nil, startWith)
	case reflect.Slice:
		slice := make([]string, 0)
		for i := 0; i < value.Len(); i++ {
			arguments, err := listArguments(value.Index(i).Interface(), resource, startWith)
			if err != nil {
				return nil, err
			}
			slice = append(slice, arguments...)
		}
		return slice, nil
	default:
		if resource != nil {
			return nil, fmt.Errorf("resource of %s kind is not supported", value.Kind())
		}
	}

	var val string

	switch v := object.(type) {
	case graphql.String:
		val = string(v)
	case graphql.Boolean:
		val = strconv.FormatBool(bool(v))
	case graphql.Int:
		val = fmt.Sprintf("%d", v)
	case graphql.Float:
		val = fmt.Sprintf("%f", v)
	default:
		return nil, fmt.Errorf("unsupported value: %#v", v)
	}

	if strings.HasPrefix(val, startWith) {
		return []string{val}, nil
	}
	return nil, nil
}

func (c *CtrlClient) CompleteQuery(namespace string, completeLeaves bool) ([]string, error) {
	namespaceType, err := c.Schema.MustGetType(NamespaceType)
	if err != nil {
		return nil, err
	}

	completion, err := c.completeQuery(NewAutoCompleteContext(namespace, 6, completeLeaves), namespaceType)
	if err != nil {
		return nil, err
	}

	return completion, nil
}

// accepts ScalarKind, EnumKind and ObjectKind
func (c *CtrlClient) completeQuery(ctx *AutoCompleteContext, root *Type) ([]string, error) {
	if ctx.IsComplete() {
		return nil, nil
	}

	switch root.Kind {
	case ScalarKind, EnumKind:
		return nil, nil
	case ListKind, NonNullKind:
		return nil, fmt.Errorf("type is not supported to complete: %#v", root)
	}

	var trunks, leaves []string
	for _, field := range root.Fields {
		subType, err := c.Schema.resolve(&field.Type)
		if err != nil {
			return nil, err
		}

		if ctx.Visited(string(subType.Name)) {
			continue
		}

		if len(field.Args) == 0 {
			subQueries, err := c.completeQuery(ctx.Next(string(subType.Name), string(field.Name), ""), subType)
			if err != nil {
				return nil, err
			}

			if subQueries == nil {
				// this field is a leaf
				// or rearching the max recursion levels
				leaves = append(leaves, string(field.Name))
				continue
			}

			for _, subQuery := range subQueries {
				trunks = append(trunks, strings.Join([]string{string(field.Name), subQuery}, "/"))
			}
			continue
		}

		args, err := c.ListArguments(append(ctx.query, string(field.Name)), string(field.Args[0].Name))
		if err != nil {
			return nil, err
		}

		for _, arg := range args {
			subQueries, err := c.completeQuery(ctx.Next(string(subType.Name), string(field.Name), arg), subType)
			if err != nil {
				return nil, err
			}

			for _, subQuery := range subQueries {
				trunks = append(trunks, strings.Join([]string{string(field.Name), arg, subQuery}, "/"))
			}
			continue
		}
	}

	var queries []string
	if !ctx.completeLeaves {
		queries = append(queries, leaves...)
	} else {
		for _, leafPrmt := range fullPermutation(leaves) {
			queries = append(queries, strings.Join(leafPrmt, ","))
		}
	}

	queries = append(queries, trunks...)
	return queries, nil
}

func fullPermutation(strs []string) [][]string {
	var results [][]string

	for i := range strs[1:] {
		substrs := make([]string, i+1)
		var (
			n = len(strs)    // length of set
			k = len(substrs) // length of subset
		)

		c := comb.New(n, k)
		p := prmt.New(prmt.StringSlice(substrs))

		for c.Next() {
			// fill substrs by indexes
			for subsetIndex, setIndex := range c.Indexes() {
				substrs[subsetIndex] = strs[setIndex]
			}

			for p.Next() {
				results = append(results, append(make([]string, 0, len(substrs)), substrs...))
			}
		}
	}

	return results
}
