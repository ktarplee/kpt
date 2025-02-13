// Copyright 2022 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package porch

import (
	"context"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
)

// SimpleRESTUpdateStrategy is similar to rest.RESTUpdateStrategy, though only contains
// methods currently required.
type SimpleRESTUpdateStrategy interface {
	PrepareForUpdate(ctx context.Context, obj, old runtime.Object)
	ValidateUpdate(ctx context.Context, obj, old runtime.Object) field.ErrorList
	Canonicalize(obj runtime.Object)
}

type NoopUpdateStrategy struct{}

func (s NoopUpdateStrategy) PrepareForUpdate(ctx context.Context, obj, old runtime.Object) {}
func (s NoopUpdateStrategy) ValidateUpdate(ctx context.Context, obj, old runtime.Object) field.ErrorList {
	return nil
}
func (s NoopUpdateStrategy) Canonicalize(obj runtime.Object) {}
