// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package envoyfault

// Envoy runtime keys for the HTTP fault filter. The default key prefix is
// "fault.http"; we don't override stat_prefix on the bootstrap so these
// canonical names are what Envoy looks up.
//
// Reference: envoyproxy.io/docs/envoy/latest/configuration/http/http_filters/fault_filter
const (
	// runtimeKeyDelayPercent overrides delay.percentage. Integer 0-100.
	runtimeKeyDelayPercent = "fault.http.delay.fixed_delay_percent"
	// runtimeKeyDelayDuration overrides delay.fixed_delay. Milliseconds.
	runtimeKeyDelayDuration = "fault.http.delay.fixed_duration"
	// runtimeKeyAbortPercent overrides abort.percentage. Integer 0-100.
	runtimeKeyAbortPercent = "fault.http.abort.abort_percent"
	// runtimeKeyAbortStatus overrides abort.http_status. Integer (e.g. 503).
	runtimeKeyAbortStatus = "fault.http.abort.http_status"
)
