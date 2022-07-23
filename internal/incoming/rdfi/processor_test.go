// Licensed to The Moov Authors under one or more contributor
// license agreements. See the NOTICE file distributed with
// this work for additional information regarding copyright
// ownership. The Moov Authors licenses this file to you under
// the Apache License, Version 2.0 (the "License"); you may
// not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied.  See the License for the
// specific language governing permissions and limitations
// under the License.

package rdfi

import (
	"io/ioutil"
	"path/filepath"
	"testing"

	"github.com/moov-io/achgateway/internal/audittrail"
)

func TestProcessor__process(t *testing.T) {
	dir := t.TempDir()
	if err := ioutil.WriteFile(filepath.Join(dir, "invalid.ach"), []byte("invalid-ach-file"), 0600); err != nil {
		t.Fatal(err)
	}

	processors := SetupProcessors(&MockProcessor{})
	auditSaver := &AuditSaver{
		storage:  &audittrail.MockStorage{},
		hostname: "ftp.foo.com",
	}

	// By reading a file without ACH FileHeaders we still want to try and process
	// Batches inside of it if any are found, so reading this kind of file shouldn't
	// return an error from reading the file.
	if err := processDir(dir, auditSaver, processors); err != nil {
		t.Error(err)
	}
}
