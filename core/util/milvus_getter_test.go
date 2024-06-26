/*
 * Licensed to the LF AI & Data foundation under one
 * or more contributor license agreements. See the NOTICE file
 * distributed with this work for additional information
 * regarding copyright ownership. The ASF licenses this file
 * to you under the Apache License, Version 2.0 (the
 * "License"); you may not use this file except in compliance
 * with the License. You may obtain a copy of the License at
 * //
 *     http://www.apache.org/licenses/LICENSE-2.0
 * //
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package util

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/milvus-io/milvus-proto/go-api/v2/msgpb"
)

func TestGetCollectionNameFromRequest(t *testing.T) {
	type args struct {
		req any
	}
	tests := []struct {
		name  string
		args  args
		want  string
		want1 bool
	}{
		{
			name: "success",
			args: args{
				req: &msgpb.InsertRequest{
					CollectionName: "hello",
				},
			},
			want:  "hello",
			want1: true,
		},
		{
			name: "success",
			args: args{
				req: "aaa",
			},
			want:  "",
			want1: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, got1 := GetCollectionNameFromRequest(tt.args.req)
			assert.Equalf(t, tt.want, got, "GetCollectionNameFromRequest(%v)", tt.args.req)
			assert.Equalf(t, tt.want1, got1, "GetCollectionNameFromRequest(%v)", tt.args.req)
		})
	}
}
