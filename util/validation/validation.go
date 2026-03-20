/*
Copyright 2026 The Kubernetes Authors.

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

package validation

import (
	"strings"

	corev1 "k8s.io/api/core/v1"
	unversionedvalidation "k8s.io/apimachinery/pkg/apis/meta/v1/validation"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
)

func validateTaintEffect(effect *corev1.TaintEffect, allowEmpty bool, fldPath *field.Path) field.ErrorList {
	if !allowEmpty && len(*effect) == 0 {
		return field.ErrorList{field.Required(fldPath, "")}
	}

	allErrors := field.ErrorList{}
	switch *effect {
	case corev1.TaintEffectNoSchedule, corev1.TaintEffectPreferNoSchedule, corev1.TaintEffectNoExecute:
	default:
		validValues := []corev1.TaintEffect{
			corev1.TaintEffectNoSchedule,
			corev1.TaintEffectPreferNoSchedule,
			corev1.TaintEffectNoExecute,
		}
		allErrors = append(allErrors, field.NotSupported(fldPath, *effect, validValues))
	}
	return allErrors
}

func ValidateNodeTaints(taints []corev1.Taint, fldPath *field.Path) field.ErrorList {
	allErrors := field.ErrorList{}

	uniqueTaints := map[corev1.TaintEffect]sets.Set[string]{}

	for i, currTaint := range taints {
		idxPath := fldPath.Index(i)
		allErrors = append(allErrors, unversionedvalidation.ValidateLabelName(currTaint.Key, idxPath.Child("key"))...)
		if errs := validation.IsValidLabelValue(currTaint.Value); len(errs) != 0 {
			allErrors = append(allErrors, field.Invalid(idxPath.Child("value"), currTaint.Value, strings.Join(errs, ";")))
		}
		allErrors = append(allErrors, validateTaintEffect(&currTaint.Effect, false, idxPath.Child("effect"))...)

		if len(uniqueTaints[currTaint.Effect]) > 0 && uniqueTaints[currTaint.Effect].Has(currTaint.Key) {
			duplicatedError := field.Duplicate(idxPath, currTaint)
			duplicatedError.Detail = "taints must be unique by key and effect pair"
			allErrors = append(allErrors, duplicatedError)
			continue
		}

		if len(uniqueTaints[currTaint.Effect]) == 0 {
			uniqueTaints[currTaint.Effect] = sets.Set[string]{}
		}
		uniqueTaints[currTaint.Effect].Insert(currTaint.Key)
	}
	return allErrors
}
