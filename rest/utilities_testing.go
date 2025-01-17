/*
Copyright 2017-Present Couchbase, Inc.

Use of this software is governed by the Business Source License included in
the file licenses/BSL-Couchbase.txt.  As of the Change Date specified in that
file, in accordance with the Business Source License, use of this software will
be governed by the Apache License, Version 2.0, included in the file
licenses/APL2.txt.
*/

package rest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"runtime/debug"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/couchbase/go-blip"
	sgbucket "github.com/couchbase/sg-bucket"
	"github.com/couchbase/sync_gateway/auth"
	"github.com/couchbase/sync_gateway/base"
	"github.com/couchbase/sync_gateway/channels"
	"github.com/couchbase/sync_gateway/db"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"
)

// Testing utilities that have been included in the rest package so that they
// are available to any package that imports rest.  (if they were in a _test.go
// file, they wouldn't be publicly exported to other packages)

// RestTesterConfig represents configuration for sync gateway
type RestTesterConfig struct {
	GuestEnabled                    bool                        // If this is true, Admin Party is in full effect
	SyncFn                          string                      // put the sync() function source in here (optional)
	DatabaseConfig                  *DatabaseConfig             // Supports additional config options.  BucketConfig, Name, Sync, Unsupported will be ignored (overridden)
	MutateStartupConfig             func(config *StartupConfig) // Function to mutate the startup configuration before the server context gets created. This overrides options the RT sets.
	InitSyncSeq                     uint64                      // If specified, initializes _sync:seq on bucket creation.  Not supported when running against walrus
	EnableNoConflictsMode           bool                        // Enable no-conflicts mode.  By default, conflicts will be allowed, which is the default behavior
	EnableUserQueries               bool                        // Enable the feature-flag for user N1QL/etc queries
	CustomTestBucket                *base.TestBucket            // If set, use this bucket instead of requesting a new one.
	leakyBucketConfig               *base.LeakyBucketConfig     // Set to create and use a leaky bucket on the RT and DB. A test bucket cannot be passed in if using this option.
	adminInterface                  string                      // adminInterface overrides the default admin interface.
	SgReplicateEnabled              bool                        // SgReplicateManager disabled by default for RestTester
	HideProductInfo                 bool
	AdminInterfaceAuthentication    bool
	metricsInterfaceAuthentication  bool
	enableAdminAuthPermissionsCheck bool
	useTLSServer                    bool // If true, TLS will be required for communications with CBS. Default: false
	persistentConfig                bool
	groupID                         *string
	serverless                      bool // Runs SG in serverless mode. Must be used in conjunction with persistent config
}

// RestTester provides a fake server for testing endpoints
type RestTester struct {
	*RestTesterConfig
	TB                      testing.TB
	TestBucket              *base.TestBucket
	RestTesterServerContext *ServerContext
	AdminHandler            http.Handler
	adminHandlerOnce        sync.Once
	PublicHandler           http.Handler
	publicHandlerOnce       sync.Once
	MetricsHandler          http.Handler
	metricsHandlerOnce      sync.Once
	closed                  bool
}

func NewRestTester(tb testing.TB, restConfig *RestTesterConfig) *RestTester {
	var rt RestTester
	if tb == nil {
		panic("tester parameter cannot be nil")
	}
	rt.TB = tb
	if restConfig != nil {
		rt.RestTesterConfig = restConfig
	} else {
		rt.RestTesterConfig = &RestTesterConfig{}
	}
	rt.RestTesterConfig.useTLSServer = base.ServerIsTLS(base.UnitTestUrl())
	return &rt
}

func (rt *RestTester) Bucket() base.Bucket {
	if rt.TB == nil {
		panic("RestTester not properly initialized please use NewRestTester function")
	} else if rt.closed {
		panic("RestTester was closed!")
	}

	if rt.TestBucket != nil {
		return rt.TestBucket.Bucket
	}

	// If we have a TestBucket defined on the RestTesterConfig, use that instead of requesting a new one.
	testBucket := rt.RestTesterConfig.CustomTestBucket
	if testBucket == nil {
		testBucket = base.GetTestBucket(rt.TB)
		if rt.leakyBucketConfig != nil {
			leakyConfig := *rt.leakyBucketConfig
			// Ignore closures to avoid double closing panics
			leakyConfig.IgnoreClose = true
			testBucket = testBucket.LeakyBucketClone(leakyConfig)
		}
	} else if rt.leakyBucketConfig != nil {
		rt.TB.Fatalf("A passed in TestBucket cannot be used on the RestTester when defining a leakyBucketConfig")
	}
	rt.TestBucket = testBucket

	if rt.InitSyncSeq > 0 {
		log.Printf("Initializing %s to %d", base.SyncSeqKey, rt.InitSyncSeq)
		_, incrErr := testBucket.Incr(base.SyncSeqKey, rt.InitSyncSeq, rt.InitSyncSeq, 0)
		if incrErr != nil {
			rt.TB.Fatalf("Error initializing %s in test bucket: %v", base.SyncSeqKey, incrErr)
		}
	}

	corsConfig := &CORSConfig{
		Origin:      []string{"http://example.com", "*", "http://staging.example.com"},
		LoginOrigin: []string{"http://example.com"},
		Headers:     []string{},
		MaxAge:      1728000,
	}

	adminInterface := &DefaultAdminInterface
	if rt.RestTesterConfig.adminInterface != "" {
		adminInterface = &rt.RestTesterConfig.adminInterface
	}

	sc := DefaultStartupConfig("")

	username, password, _ := testBucket.BucketSpec.Auth.GetCredentials()

	// Disable config polling to avoid test flakiness and increase control of timing.
	// Rely on on-demand config fetching for consistency.
	sc.Bootstrap.ConfigUpdateFrequency = base.NewConfigDuration(0)

	sc.Bootstrap.Server = testBucket.BucketSpec.Server
	sc.Bootstrap.Username = username
	sc.Bootstrap.Password = password
	sc.API.AdminInterface = *adminInterface
	sc.API.CORS = corsConfig
	sc.API.HideProductVersion = base.BoolPtr(rt.RestTesterConfig.HideProductInfo)
	sc.DeprecatedConfig = &DeprecatedConfig{Facebook: &FacebookConfigLegacy{}}
	sc.API.AdminInterfaceAuthentication = &rt.AdminInterfaceAuthentication
	sc.API.MetricsInterfaceAuthentication = &rt.metricsInterfaceAuthentication
	sc.API.EnableAdminAuthenticationPermissionsCheck = &rt.enableAdminAuthPermissionsCheck
	sc.Bootstrap.UseTLSServer = &rt.RestTesterConfig.useTLSServer
	sc.Bootstrap.ServerTLSSkipVerify = base.BoolPtr(base.TestTLSSkipVerify())
	sc.Unsupported.Serverless.Enabled = &rt.serverless
	if rt.serverless {
		if !rt.persistentConfig {
			rt.TB.Fatalf("Persistent config must be used when running in serverless mode")
		}
		sc.BucketCredentials = map[string]*base.CredentialsConfig{
			testBucket.GetName(): {
				Username: base.TestClusterUsername(),
				Password: base.TestClusterPassword(),
			},
		}
	}

	if rt.RestTesterConfig.groupID != nil {
		sc.Bootstrap.ConfigGroupID = *rt.RestTesterConfig.groupID
	} else if rt.RestTesterConfig.persistentConfig {
		// If running in persistent config mode, the database has to be manually created. If the db name is the same as a
		// past tests db name, a db already exists error could happen if the past tests bucket is still flushing. Prevent this
		// by setting the group ID as the current test name by default.
		sc.Bootstrap.ConfigGroupID = fmt.Sprintf("%x", sha256.Sum256([]byte(rt.TB.Name())))
	}

	sc.Unsupported.UserQueries = base.BoolPtr(rt.EnableUserQueries)

	if rt.MutateStartupConfig != nil {
		rt.MutateStartupConfig(&sc)
	}

	// Allow EE-only config even in CE for testing using group IDs.
	if err := sc.Validate(true); err != nil {
		panic("invalid RestTester StartupConfig: " + err.Error())
	}

	// Post-validation, we can lower the bcrypt cost beyond SG limits to reduce test runtime.
	sc.Auth.BcryptCost = bcrypt.MinCost

	rt.RestTesterServerContext = NewServerContext(base.TestCtx(rt.TB), &sc, rt.RestTesterConfig.persistentConfig)
	ctx := rt.Context()

	if !base.ServerIsWalrus(sc.Bootstrap.Server) {
		// Copy any testbucket cert info into boostrap server config
		// Required as present for X509 tests there is no way to pass this info to the bootstrap server context with a
		// RestTester directly - Should hopefully be alleviated by CBG-1460
		sc.Bootstrap.CACertPath = testBucket.BucketSpec.CACertPath
		sc.Bootstrap.X509CertPath = testBucket.BucketSpec.Certpath
		sc.Bootstrap.X509KeyPath = testBucket.BucketSpec.Keypath

		rt.TestBucket.BucketSpec.TLSSkipVerify = base.TestTLSSkipVerify()

		if err := rt.RestTesterServerContext.initializeCouchbaseServerConnections(ctx); err != nil {
			panic("Couldn't initialize Couchbase Server connection: " + err.Error())
		}
	}

	// Copy this startup config at this point into initial startup config
	err := base.DeepCopyInefficient(&rt.RestTesterServerContext.initialStartupConfig, &sc)
	if err != nil {
		rt.TB.Fatalf("Unable to copy initial startup config: %v", err)
	}

	// tests must create their own databases in persistent mode
	if !rt.persistentConfig {
		useXattrs := base.TestUseXattrs()

		if rt.DatabaseConfig == nil {
			// If no db config was passed in, create one
			rt.DatabaseConfig = &DatabaseConfig{}
		}

		if rt.DatabaseConfig.UseViews == nil {
			rt.DatabaseConfig.UseViews = base.BoolPtr(base.TestsDisableGSI())
		}

		collection, collectionErr := base.AsCollection(rt.TestBucket)
		if collectionErr == nil && rt.DatabaseConfig.Scopes == nil && collection.Spec.Scope != nil && collection.Spec.Collection != nil {
			rt.DatabaseConfig.Scopes = ScopesConfig{
				*collection.Spec.Scope: ScopeConfig{
					Collections: map[string]CollectionConfig{
						*collection.Spec.Collection: {},
					},
				},
			}
		}
		// numReplicas set to 0 for test buckets, since it should assume that there may only be one indexing node.
		numReplicas := uint(0)
		rt.DatabaseConfig.NumIndexReplicas = &numReplicas

		rt.DatabaseConfig.Bucket = &testBucket.BucketSpec.BucketName
		rt.DatabaseConfig.Username = username
		rt.DatabaseConfig.Password = password
		rt.DatabaseConfig.CACertPath = testBucket.BucketSpec.CACertPath
		rt.DatabaseConfig.CertPath = testBucket.BucketSpec.Certpath
		rt.DatabaseConfig.KeyPath = testBucket.BucketSpec.Keypath
		rt.DatabaseConfig.Name = "db"
		rt.DatabaseConfig.Sync = &rt.SyncFn
		rt.DatabaseConfig.EnableXattrs = &useXattrs
		if rt.EnableNoConflictsMode {
			boolVal := false
			rt.DatabaseConfig.AllowConflicts = &boolVal
		}

		rt.DatabaseConfig.SGReplicateEnabled = base.BoolPtr(rt.RestTesterConfig.SgReplicateEnabled)

		autoImport, _ := rt.DatabaseConfig.AutoImportEnabled()
		if rt.DatabaseConfig.ImportPartitions == nil && base.TestUseXattrs() && base.IsEnterpriseEdition() && autoImport {
			// Speed up test setup - most tests don't need more than one partition given we only have one node
			rt.DatabaseConfig.ImportPartitions = base.Uint16Ptr(1)
		}

		if rt.leakyBucketConfig != nil {
			// Scopes and collections have to be set on the bucket being passed in for the db to use.
			// WIP: Collections Phase 1 - Grab just one scope/collection from the defined set.
			// Phase 2 (multi collection) means DatabaseContext needs a set of BucketSpec/Collections, not just one...
			var scope, collection *string
			for scopeName, scopeConfig := range rt.RestTesterConfig.DatabaseConfig.Scopes {
				scope = &scopeName
				for collectionName := range scopeConfig.Collections {
					collection = &collectionName
					break
				}
			}
			if scope != nil && collection != nil {
				collectionBucket, err := base.AsCollection(testBucket.Bucket)
				if err != nil {
					rt.TB.Fatalf("Could not get collection from bucket with type %T: %v", testBucket.Bucket, err)
				}

				collectionBucket.Spec.Scope = scope
				collectionBucket.Spec.Collection = collection
				collectionBucket.Collection = collectionBucket.Collection.Bucket().Scope(*scope).Collection(*collection)
			}

			_, err = rt.RestTesterServerContext.AddDatabaseFromConfigWithBucket(ctx, rt.TB, *rt.DatabaseConfig, testBucket.Bucket)
		} else {
			_, err = rt.RestTesterServerContext.AddDatabaseFromConfig(ctx, *rt.DatabaseConfig)
		}

		if err != nil {
			rt.TB.Fatalf("Error from AddDatabaseFromConfig: %v", err)
		}
		ctx = rt.Context() // get new ctx with db info before passing it down

		// Update the testBucket Bucket to the one associated with the database context.  The new (dbContext) bucket
		// will be closed when the rest tester closes the server context. The original bucket will be closed using the
		// testBucket's closeFn
		rt.TestBucket.Bucket = rt.RestTesterServerContext.Database(ctx, "db").Bucket

		if rt.DatabaseConfig.Guest == nil {
			if err := rt.SetAdminParty(rt.GuestEnabled); err != nil {
				rt.TB.Fatalf("Error from SetAdminParty %v", err)
			}
		}
	}

	// PostStartup (without actually waiting 5 seconds)
	close(rt.RestTesterServerContext.hasStarted)
	return rt.TestBucket.Bucket
}

// LeakyBucket gets the bucket from the RestTester as a leaky bucket allowing for callbacks to be set on the fly.
// The RestTester must have been set up to create and use a leaky bucket by setting leakyBucketConfig in the RT
// config when calling NewRestTester.
func (rt *RestTester) LeakyBucket() *base.LeakyBucket {
	if rt.leakyBucketConfig == nil {
		rt.TB.Fatalf("Cannot get leaky bucket when leakyBucketConfig was not set on RestTester initialisation")
	}
	leakyBucket, ok := base.AsLeakyBucket(rt.Bucket())
	if !ok {
		rt.TB.Fatalf("Could not get bucket (type %T) as a leaky bucket", rt.Bucket())
	}
	return leakyBucket
}

func (rt *RestTester) ServerContext() *ServerContext {
	rt.Bucket()
	return rt.RestTesterServerContext
}

// CreateDatabase is a utility function to create a database through the REST API
func (rt *RestTester) CreateDatabase(dbName string, config DbConfig) (*TestResponse, error) {
	dbcJSON, err := base.JSONMarshal(config)
	if err != nil {
		return nil, err
	}
	resp := rt.SendAdminRequest(http.MethodPut, fmt.Sprintf("/%s/", dbName), string(dbcJSON))
	return resp, nil
}

// ReplaceDbConfig is a utility function to replace a database config through the REST API
func (rt *RestTester) ReplaceDbConfig(dbName string, config DbConfig) (*TestResponse, error) {
	dbcJSON, err := base.JSONMarshal(config)
	if err != nil {
		return nil, err
	}
	resp := rt.SendAdminRequest(http.MethodPut, fmt.Sprintf("/%s/_config", dbName), string(dbcJSON))
	return resp, nil
}

// UpsertDbConfig is a utility function to upsert a database through the REST API
func (rt *RestTester) UpsertDbConfig(dbName string, config DbConfig) (*TestResponse, error) {
	dbcJSON, err := base.JSONMarshal(config)
	if err != nil {
		return nil, err
	}
	resp := rt.SendAdminRequest(http.MethodPost, fmt.Sprintf("/%s/_config", dbName), string(dbcJSON))
	return resp, nil
}

// Returns first database found for server context.
func (rt *RestTester) GetDatabase() *db.DatabaseContext {

	for _, database := range rt.ServerContext().AllDatabases() {
		return database
	}
	return nil
}

func (rt *RestTester) MustWaitForDoc(docid string, t testing.TB) {
	err := rt.WaitForDoc(docid)
	assert.NoError(t, err)
}

func (rt *RestTester) WaitForDoc(docid string) (err error) {
	seq, err := rt.SequenceForDoc(docid)
	if err != nil {
		return err
	}
	return rt.WaitForSequence(seq)
}

func (rt *RestTester) SequenceForDoc(docid string) (seq uint64, err error) {
	database := rt.GetDatabase()
	if database == nil {
		return 0, fmt.Errorf("No database found")
	}
	doc, err := database.GetDocument(base.TestCtx(rt.TB), docid, db.DocUnmarshalAll)
	if err != nil {
		return 0, err
	}
	return doc.Sequence, nil
}

// Wait for sequence to be buffered by the channel cache
func (rt *RestTester) WaitForSequence(seq uint64) error {
	database := rt.GetDatabase()
	if database == nil {
		return fmt.Errorf("No database found")
	}
	return database.WaitForSequence(base.TestCtx(rt.TB), seq)
}

func (rt *RestTester) WaitForPendingChanges() error {
	database := rt.GetDatabase()
	if database == nil {
		return fmt.Errorf("No database found")
	}
	return database.WaitForPendingChanges(base.TestCtx(rt.TB))
}

func (rt *RestTester) SetAdminParty(partyTime bool) error {
	ctx := rt.Context()
	a := rt.ServerContext().Database(ctx, "db").Authenticator(ctx)
	guest, err := a.GetUser("")
	if err != nil {
		return err
	}
	guest.SetDisabled(!partyTime)
	var chans channels.TimedSet
	if partyTime {
		chans = channels.AtSequence(base.SetOf(channels.UserStarChannel), 1)
	}
	guest.SetExplicitChannels(chans, 1)
	return a.Save(guest)
}

func (rt *RestTester) Close() {
	if rt.TB == nil {
		panic("RestTester not properly initialized please use NewRestTester function")
	}
	ctx := rt.Context() // capture ctx before closing rt
	rt.closed = true
	if rt.RestTesterServerContext != nil {
		rt.RestTesterServerContext.Close(ctx)
	}
	if rt.TestBucket != nil {
		rt.TestBucket.Close()
		rt.TestBucket = nil
	}
}

func (rt *RestTester) SendRequest(method, resource string, body string) *TestResponse {
	return rt.Send(Request(method, resource, body))
}

func (rt *RestTester) SendRequestWithHeaders(method, resource string, body string, headers map[string]string) *TestResponse {
	req := Request(method, resource, body)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return rt.Send(req)
}

func (rt *RestTester) SendUserRequestWithHeaders(method, resource string, body string, headers map[string]string, username string, password string) *TestResponse {
	req := Request(method, resource, body)
	req.SetBasicAuth(username, password)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return rt.Send(req)
}

func (rt *RestTester) SendAdminRequestWithAuth(method, resource string, body string, username string, password string) *TestResponse {
	input := bytes.NewBufferString(body)
	request, err := http.NewRequest(method, "http://localhost"+resource, input)
	require.NoError(rt.TB, err)

	request.SetBasicAuth(username, password)

	response := &TestResponse{ResponseRecorder: httptest.NewRecorder(), Req: request}
	response.Code = 200 // doesn't seem to be initialized by default; filed Go bug #4188

	rt.TestAdminHandler().ServeHTTP(response, request)
	return response
}

func (rt *RestTester) Send(request *http.Request) *TestResponse {
	response := &TestResponse{ResponseRecorder: httptest.NewRecorder(), Req: request}
	response.Code = 200 // doesn't seem to be initialized by default; filed Go bug #4188
	rt.TestPublicHandler().ServeHTTP(response, request)
	return response
}

func (rt *RestTester) TestAdminHandlerNoConflictsMode() http.Handler {
	rt.EnableNoConflictsMode = true
	return rt.TestAdminHandler()
}

func (rt *RestTester) TestAdminHandler() http.Handler {
	rt.adminHandlerOnce.Do(func() {
		rt.AdminHandler = CreateAdminHandler(rt.ServerContext())
	})
	return rt.AdminHandler
}

func (rt *RestTester) TestPublicHandler() http.Handler {
	rt.publicHandlerOnce.Do(func() {
		rt.PublicHandler = CreatePublicHandler(rt.ServerContext())
	})
	return rt.PublicHandler
}

func (rt *RestTester) TestMetricsHandler() http.Handler {
	rt.metricsHandlerOnce.Do(func() {
		rt.MetricsHandler = CreateMetricHandler(rt.ServerContext())
	})
	return rt.MetricsHandler
}

type ChangesResults struct {
	Results  []db.ChangeEntry
	Last_Seq interface{}
}

func (cr ChangesResults) requireDocIDs(t testing.TB, docIDs []string) {
	require.Equal(t, len(docIDs), len(cr.Results))
	for _, docID := range docIDs {
		var found bool
		for _, changeEntry := range cr.Results {
			if changeEntry.ID == docID {
				found = true
				break
			}
		}
		require.True(t, found)
	}
}

func (rt *RestTester) CreateWaitForChangesRetryWorker(numChangesExpected int, changesURL, username string, useAdminPort bool) (worker base.RetryWorker) {

	waitForChangesWorker := func() (shouldRetry bool, err error, value interface{}) {

		var changes ChangesResults
		var response *TestResponse

		if useAdminPort {
			response = rt.SendAdminRequest("GET", changesURL, "")

		} else {
			response = rt.Send(RequestByUser("GET", changesURL, "", username))
		}
		err = base.JSONUnmarshal(response.Body.Bytes(), &changes)
		if err != nil {
			return false, err, nil
		}
		if len(changes.Results) < numChangesExpected {
			// not enough results, retry
			return true, nil, nil
		}
		// If it made it this far, there is no errors and it got enough changes
		return false, nil, changes
	}

	return waitForChangesWorker

}

func (rt *RestTester) WaitForChanges(numChangesExpected int, changesURL, username string, useAdminPort bool) (
	changes ChangesResults,
	err error) {

	waitForChangesWorker := rt.CreateWaitForChangesRetryWorker(numChangesExpected, changesURL, username, useAdminPort)

	sleeper := base.CreateSleeperFunc(200, 100)

	err, changesVal := base.RetryLoop("Wait for changes", waitForChangesWorker, sleeper)
	if err != nil {
		return changes, err
	}

	if changesVal == nil {
		return changes, fmt.Errorf("Got nil value for changes")
	}

	if changesVal != nil {
		changes = changesVal.(ChangesResults)
	}

	return changes, nil
}

// WaitForCondition runs a retry loop that evaluates the provided function, and terminates
// when the function returns true.
func (rt *RestTester) WaitForCondition(successFunc func() bool) error {
	return rt.WaitForConditionWithOptions(successFunc, 200, 100)
}

func (rt *RestTester) WaitForConditionWithOptions(successFunc func() bool, maxNumAttempts, timeToSleepMs int) error {
	waitForSuccess := func() (shouldRetry bool, err error, value interface{}) {
		if successFunc() {
			return false, nil, nil
		}
		return true, nil, nil
	}

	sleeper := base.CreateSleeperFunc(maxNumAttempts, timeToSleepMs)
	err, _ := base.RetryLoop("Wait for condition options", waitForSuccess, sleeper)
	if err != nil {
		return err
	}

	return nil
}

func (rt *RestTester) WaitForConditionShouldRetry(conditionFunc func() (shouldRetry bool, err error, value interface{}), maxNumAttempts, timeToSleepMs int) error {
	sleeper := base.CreateSleeperFunc(maxNumAttempts, timeToSleepMs)
	err, _ := base.RetryLoop("Wait for condition options", conditionFunc, sleeper)
	if err != nil {
		return err
	}

	return nil
}

func (rt *RestTester) SendAdminRequest(method, resource string, body string) *TestResponse {
	input := bytes.NewBufferString(body)
	request, err := http.NewRequest(method, "http://localhost"+resource, input)
	require.NoError(rt.TB, err)

	response := &TestResponse{ResponseRecorder: httptest.NewRecorder(), Req: request}
	response.Code = 200 // doesn't seem to be initialized by default; filed Go bug #4188

	rt.TestAdminHandler().ServeHTTP(response, request)
	return response
}

func (rt *RestTester) WaitForNUserViewResults(numResultsExpected int, viewUrlPath string, user auth.User, password string) (viewResult sgbucket.ViewResult, err error) {
	return rt.WaitForNViewResults(numResultsExpected, viewUrlPath, user, password)
}

func (rt *RestTester) WaitForNAdminViewResults(numResultsExpected int, viewUrlPath string) (viewResult sgbucket.ViewResult, err error) {
	return rt.WaitForNViewResults(numResultsExpected, viewUrlPath, nil, "")
}

// Wait for a certain number of results to be returned from a view query
// viewUrlPath: is the path to the view, including the db name.  Eg: "/db/_design/foo/_view/bar"
func (rt *RestTester) WaitForNViewResults(numResultsExpected int, viewUrlPath string, user auth.User, password string) (viewResult sgbucket.ViewResult, err error) {

	worker := func() (shouldRetry bool, err error, value interface{}) {
		var response *TestResponse
		if user != nil {
			request, _ := http.NewRequest("GET", viewUrlPath, nil)
			request.SetBasicAuth(user.Name(), password)
			response = rt.Send(request)
		} else {
			response = rt.SendAdminRequest("GET", viewUrlPath, ``)
		}

		// If the view is undefined, it might be a race condition where the view is still being created
		// See https://github.com/couchbase/sync_gateway/issues/3570#issuecomment-390487982
		if strings.Contains(response.Body.String(), "view_undefined") {
			base.InfofCtx(rt.Context(), base.KeyAll, "view_undefined error: %v.  Retrying", response.Body.String())
			return true, nil, nil
		}

		if response.Code != 200 {
			return false, fmt.Errorf("Got response code: %d from view call.  Expected 200", response.Code), sgbucket.ViewResult{}
		}
		var result sgbucket.ViewResult
		_ = base.JSONUnmarshal(response.Body.Bytes(), &result)

		if len(result.Rows) >= numResultsExpected {
			// Got enough results, break out of retry loop
			return false, nil, result
		}

		// Not enough results, retry
		return true, nil, sgbucket.ViewResult{}

	}

	description := fmt.Sprintf("Wait for %d view results for query to %v", numResultsExpected, viewUrlPath)
	sleeper := base.CreateSleeperFunc(200, 100)
	err, returnVal := base.RetryLoop(description, worker, sleeper)

	if err != nil {
		return sgbucket.ViewResult{}, err
	}

	return returnVal.(sgbucket.ViewResult), nil

}

// Waits for view to be defined on the server.  Used to avoid view_undefined errors.
func (rt *RestTester) WaitForViewAvailable(viewURLPath string) (err error) {

	worker := func() (shouldRetry bool, err error, value interface{}) {
		response := rt.SendAdminRequest("GET", viewURLPath, ``)

		if response.Code == 200 {
			return false, nil, nil
		}

		// Views unavailable, retry
		if response.Code == 500 {
			log.Printf("Error waiting for view to be available....will retry: %s", response.Body.Bytes())
			return true, fmt.Errorf("500 error"), nil
		}

		// Unexpected error, return
		return false, fmt.Errorf("Unexpected error response code while waiting for view available: %v", response.Code), nil

	}

	description := "Wait for view readiness"
	sleeper := base.CreateSleeperFunc(200, 100)
	err, _ = base.RetryLoop(description, worker, sleeper)

	return err

}

func (rt *RestTester) GetDBState() string {
	var body db.Body
	resp := rt.SendAdminRequest("GET", "/db/", "")
	RequireStatus(rt.TB, resp, 200)
	require.NoError(rt.TB, base.JSONUnmarshal(resp.Body.Bytes(), &body))
	return body["state"].(string)
}

func (rt *RestTester) WaitForDBOnline() (err error) {
	return rt.waitForDBState("Online")
}

func (rt *RestTester) waitForDBState(stateWant string) (err error) {
	var stateCurr string
	maxTries := 20

	for i := 0; i < maxTries; i++ {
		if stateCurr = rt.GetDBState(); stateCurr == stateWant {
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("given up waiting for DB state, want: %s, current: %s, attempts: %d", stateWant, stateCurr, maxTries)
}

func (rt *RestTester) SendAdminRequestWithHeaders(method, resource string, body string, headers map[string]string) *TestResponse {
	input := bytes.NewBufferString(body)
	request, _ := http.NewRequest(method, "http://localhost"+resource, input)
	for k, v := range headers {
		request.Header.Set(k, v)
	}
	response := &TestResponse{ResponseRecorder: httptest.NewRecorder(), Req: request}
	response.Code = 200 // doesn't seem to be initialized by default; filed Go bug #4188

	rt.TestAdminHandler().ServeHTTP(response, request)
	return response
}

// PutDocumentWithRevID builds a new_edits=false style put to create a revision with the specified revID.
// If parentRevID is not specified, treated as insert
func (rt *RestTester) PutDocumentWithRevID(docID string, newRevID string, parentRevID string, body db.Body) (response *TestResponse, err error) {

	requestBody := body.ShallowCopy()
	newRevGeneration, newRevDigest := db.ParseRevID(newRevID)

	revisions := make(map[string]interface{})
	revisions["start"] = newRevGeneration
	ids := []string{newRevDigest}
	if parentRevID != "" {
		_, parentDigest := db.ParseRevID(parentRevID)
		ids = append(ids, parentDigest)
	}
	revisions["ids"] = ids

	requestBody[db.BodyRevisions] = revisions
	requestBytes, err := json.Marshal(requestBody)
	if err != nil {
		return nil, err
	}
	resp := rt.SendAdminRequest(http.MethodPut, "/db/"+docID+"?new_edits=false", string(requestBytes))
	return resp, nil
}

type SimpleSync struct {
	Channels map[string]interface{}
	Rev      string
	Sequence uint64
}

type RawResponse struct {
	Sync SimpleSync `json:"_sync"`
}

// GetDocumentSequence looks up the sequence for a document using the _raw endpoint.
// Used by tests that need to validate sequences (for grants, etc)
func (rt *RestTester) GetDocumentSequence(key string) (sequence uint64) {
	response := rt.SendAdminRequest("GET", fmt.Sprintf("/db/_raw/%s", key), "")
	if response.Code != 200 {
		return 0
	}

	var rawResponse RawResponse
	_ = base.JSONUnmarshal(response.BodyBytes(), &rawResponse)
	return rawResponse.Sync.Sequence
}

// ReplacePerBucketCredentials replaces buckets defined on StartupConfig.BucketCredentials then recreates the couchbase
// cluster to pick up the changes
func (rt *RestTester) ReplacePerBucketCredentials(config base.PerBucketCredentialsConfig) {
	rt.ServerContext().Config.BucketCredentials = config
	// Update the CouchbaseCluster to include the new bucket credentials
	couchbaseCluster, err := CreateCouchbaseClusterFromStartupConfig(rt.ServerContext().Config)
	require.NoError(rt.TB, err)
	rt.ServerContext().BootstrapContext.Connection = couchbaseCluster
}

func (rt *RestTester) Context() context.Context {
	ctx := base.TestCtx(rt.TB)
	if svrctx := rt.ServerContext(); svrctx != nil {
		ctx = svrctx.AddServerLogContext(ctx)
	}
	if dbctx := rt.GetDatabase(); dbctx != nil {
		ctx = dbctx.AddDatabaseLogContext(ctx)
	}
	return ctx
}

type TestResponse struct {
	*httptest.ResponseRecorder
	Req *http.Request

	bodyCache []byte
}

// BodyBytes takes a copy of the bytes in the response buffer, and saves them for future callers.
func (r TestResponse) BodyBytes() []byte {
	if r.bodyCache == nil {
		r.bodyCache = r.ResponseRecorder.Body.Bytes()
	}
	return r.bodyCache
}

func (r TestResponse) DumpBody() {
	log.Printf("%v", r.Body.String())
}

func (r TestResponse) GetRestDocument() RestDocument {
	restDoc := NewRestDocument()
	err := base.JSONUnmarshal(r.Body.Bytes(), restDoc)
	if err != nil {
		panic(fmt.Sprintf("Error parsing body into RestDocument.  Body: %s.  Err: %v", r.Body.String(), err))
	}
	return *restDoc
}

func Request(method, resource, body string) *http.Request {
	request, err := http.NewRequest(method, "http://localhost"+resource, bytes.NewBufferString(body))
	request.RequestURI = resource // This doesn't get filled in by NewRequest
	FixQuotedSlashes(request)
	if err != nil {
		panic(fmt.Sprintf("http.NewRequest failed: %v", err))
	}
	return request
}

func RequestByUser(method, resource, body, username string) *http.Request {
	r := Request(method, resource, body)
	r.SetBasicAuth(username, "letmein")
	return r
}

func RequireStatus(t testing.TB, response *TestResponse, expectedStatus int) {
	require.Equalf(t, expectedStatus, response.Code,
		"Response status %d %q (expected %d %q)\nfor %s <%s> : %s",
		response.Code, http.StatusText(response.Code),
		expectedStatus, http.StatusText(expectedStatus),
		response.Req.Method, response.Req.URL, response.Body)
}

func AssertStatus(t testing.TB, response *TestResponse, expectedStatus int) bool {
	return assert.Equalf(t, expectedStatus, response.Code,
		"Response status %d %q (expected %d %q)\nfor %s <%s> : %s",
		response.Code, http.StatusText(response.Code),
		expectedStatus, http.StatusText(expectedStatus),
		response.Req.Method, response.Req.URL, response.Body)
}

func NewSlowResponseRecorder(responseDelay time.Duration, responseRecorder *httptest.ResponseRecorder) *SlowResponseRecorder {

	responseStarted := sync.WaitGroup{}
	responseStarted.Add(1)

	responseFinished := sync.WaitGroup{}
	responseFinished.Add(1)

	return &SlowResponseRecorder{
		responseDelay:    responseDelay,
		ResponseRecorder: responseRecorder,
		responseStarted:  &responseStarted,
		responseFinished: &responseFinished,
	}

}

type SlowResponseRecorder struct {
	*httptest.ResponseRecorder
	responseDelay    time.Duration
	responseStarted  *sync.WaitGroup
	responseFinished *sync.WaitGroup
}

func (s *SlowResponseRecorder) WaitForResponseToStart() {
	s.responseStarted.Wait()
}

func (s *SlowResponseRecorder) WaitForResponseToFinish() {
	s.responseFinished.Wait()
}

func (s *SlowResponseRecorder) Write(buf []byte) (int, error) {

	s.responseStarted.Done()

	time.Sleep(s.responseDelay)

	numBytesWritten, err := s.ResponseRecorder.Write(buf)

	s.responseFinished.Done()

	return numBytesWritten, err
}

// AddDatabaseFromConfigWithBucket adds a database to the ServerContext and sets a specific bucket on the database context.
// If an existing config is found for the name, returns an error.
func (sc *ServerContext) AddDatabaseFromConfigWithBucket(ctx context.Context, tb testing.TB, config DatabaseConfig, bucket base.Bucket) (*db.DatabaseContext, error) {
	return sc.getOrAddDatabaseFromConfig(ctx, config, false, func(ctx context.Context, spec base.BucketSpec) (base.Bucket, error) {
		return bucket, nil
	})
}

// The parameters used to create a BlipTester
type BlipTesterSpec struct {

	// Run Sync Gateway in "No conflicts" mode.  Will be propgated to the underyling RestTester
	noConflictsMode bool

	// If an underlying RestTester is created, it will propagate this setting to the underlying RestTester.
	GuestEnabled bool

	// The Sync Gateway username and password to connect with.  If set, then you
	// may want to disable "Admin Party" mode, which will allow guest user access.
	// By default, the created user will have access to a single channel that matches their username.
	// If you need to grant the user access to more channels, you can override this behavior with the
	// connectingUserChannelGrants field
	connectingUsername string
	connectingPassword string

	// By default, the created user will have access to a single channel that matches their username.
	// If you need to grant the user access to more channels, you can override this behavior by specifying
	// the channels the user should have access in this string slice
	connectingUserChannelGrants []string

	// Allow tests to further customized a RestTester or re-use it across multiple BlipTesters if needed.
	// If a RestTester is passed in, certain properties of the BlipTester such as GuestEnabled will be ignored, since
	// those properties only affect the creation of the RestTester.
	// If nil, a default restTester will be created based on the properties in this spec
	// restTester *RestTester

	// Supported blipProtocols for the client to use in order of preference
	blipProtocols []string
}

// State associated with a BlipTester
// Note that it's not safe to have multiple goroutines access a single BlipTester due to the
// fact that certain methods register profile handlers on the BlipContext
type BlipTester struct {

	// The underlying RestTester which is used to bootstrap the initial blip websocket creation,
	// as well as providing a way for tests to access Sync Gateway over REST to hit admin-only endpoints
	// which are not available via blip.  Since a test may need to create multiple BlipTesters for multiple
	// user contexts, a single RestTester may be shared among multiple BlipTester instances.
	restTester *RestTester

	// This flag is used to avoid closing the contained restTester. This functionality is to avoid a double close in
	// some areas.
	avoidRestTesterClose bool

	// The blip context which contains blip related state and the sender/reciever goroutines associated
	// with this websocket connection
	blipContext *blip.Context

	// The blip sender that can be used for sending messages over the websocket connection
	sender *blip.Sender

	// Set when we receive a reply to a getCollections request. Used to verify that all messages after that contain a
	// `collection` property.
	useCollections *base.AtomicBool
}

// Close the bliptester
func (bt BlipTester) Close() {
	bt.sender.Close()
	if !bt.avoidRestTesterClose {
		bt.restTester.Close()
	}
}

// Returns database context for blipTester (assumes underlying rest tester is based on a single db - returns first it finds)
func (bt BlipTester) DatabaseContext() *db.DatabaseContext {
	dbs := bt.restTester.ServerContext().AllDatabases()
	for _, database := range dbs {
		return database
	}
	return nil
}

func NewBlipTesterFromSpecWithRT(tb testing.TB, spec *BlipTesterSpec, rt *RestTester) (blipTester *BlipTester, err error) {
	blipTesterSpec := spec
	if spec == nil {
		// Default spec
		blipTesterSpec = &BlipTesterSpec{}
	}
	blipTester, err = createBlipTesterWithSpec(tb, *blipTesterSpec, rt)
	if err != nil {
		return nil, err
	}
	blipTester.avoidRestTesterClose = true

	return blipTester, err
}

// Create a BlipTester using the default spec
func NewBlipTester(tb testing.TB) (*BlipTester, error) {
	defaultSpec := BlipTesterSpec{GuestEnabled: true}
	return NewBlipTesterFromSpec(tb, defaultSpec)
}

func NewBlipTesterFromSpec(tb testing.TB, spec BlipTesterSpec) (*BlipTester, error) {
	rtConfig := RestTesterConfig{
		EnableNoConflictsMode: spec.noConflictsMode,
		GuestEnabled:          spec.GuestEnabled,
	}
	var rt = NewRestTester(tb, &rtConfig)
	return createBlipTesterWithSpec(tb, spec, rt)
}

// Create a BlipTester using the given spec
func createBlipTesterWithSpec(tb testing.TB, spec BlipTesterSpec, rt *RestTester) (*BlipTester, error) {
	bt := &BlipTester{
		restTester:     rt,
		useCollections: base.NewAtomicBool(false),
	}

	// Since blip requests all go over the public handler, wrap the public handler with the httptest server
	publicHandler := bt.restTester.TestPublicHandler()

	if len(spec.connectingUsername) > 0 {

		// By default, the user will be granted access to a single channel equal to their username
		adminChannels := []string{spec.connectingUsername}

		// If the caller specified a list of channels to grant the user access to, then use that instead.
		if len(spec.connectingUserChannelGrants) > 0 {
			adminChannels = []string{} // empty it
			adminChannels = append(adminChannels, spec.connectingUserChannelGrants...)
		}

		// serialize admin channels to json array
		adminChannelsJson, err := base.JSONMarshal(adminChannels)
		if err != nil {
			return nil, err
		}
		adminChannelsStr := string(adminChannelsJson)

		userDocBody := fmt.Sprintf(`{"name":"%s", "password":"%s", "admin_channels":%s}`,
			spec.connectingUsername,
			spec.connectingPassword,
			adminChannelsStr,
		)
		log.Printf("Creating user: %v", userDocBody)

		// Create a user.  NOTE: this must come *after* the bt.rt.TestPublicHandler() call, otherwise it will end up getting ignored
		_ = bt.restTester.SendAdminRequest(
			"POST",
			"/db/_user/",
			userDocBody,
		)
	}

	// Create a _temporary_ test server bound to an actual port that is used to make the blip connection.
	// This is needed because the mock-based approach fails with a "Connection not hijackable" error when
	// trying to do the websocket upgrade.  Since it's only needed to setup the websocket, it can be closed
	// as soon as the websocket is established, hence the defer srv.Close() call.
	srv := httptest.NewServer(publicHandler)
	defer srv.Close()

	// Construct URL to connect to blipsync target endpoint
	destUrl := fmt.Sprintf("%s/db/_blipsync", srv.URL)
	u, err := url.Parse(destUrl)
	if err != nil {
		return nil, err
	}
	u.Scheme = "ws"

	// If protocols are not set use V3 as a V3 client would
	protocols := spec.blipProtocols
	if len(protocols) == 0 {
		protocols = []string{db.BlipCBMobileReplicationV3}
	}

	// Make BLIP/Websocket connection
	bt.blipContext, err = db.NewSGBlipContextWithProtocols(base.TestCtx(tb), "", protocols...)
	if err != nil {
		return nil, err
	}

	// Ensure that errors get correctly surfaced in tests
	bt.blipContext.FatalErrorHandler = func(err error) {
		tb.Fatalf("BLIP fatal error: %v", err)
	}
	bt.blipContext.HandlerPanicHandler = func(request, response *blip.Message, err interface{}) {
		stack := debug.Stack()
		tb.Fatalf("Panic while handling %s: %v\n%s", request.Profile(), err, string(stack))
	}

	config := blip.DialOptions{
		URL: u.String(),
	}

	if len(spec.connectingUsername) > 0 {
		config.HTTPHeader = http.Header{
			"Authorization": {"Basic " + base64.StdEncoding.EncodeToString([]byte(spec.connectingUsername+":"+spec.connectingPassword))},
		}
	}

	bt.sender, err = bt.blipContext.DialConfig(&config)
	if err != nil {
		return nil, err
	}

	return bt, nil

}

func (bt *BlipTester) SetCheckpoint(client string, checkpointRev string, body []byte) (sent bool, req *db.SetCheckpointMessage, res *db.SetCheckpointResponse, err error) {

	scm := db.NewSetCheckpointMessage()
	scm.SetCompressed(true)
	scm.SetClient(client)
	scm.SetRev(checkpointRev)
	scm.SetBody(body)

	sent = bt.sender.Send(scm.Message)
	if !sent {
		return sent, scm, nil, fmt.Errorf("Failed to send setCheckpoint for client: %v", client)
	}

	scr := &db.SetCheckpointResponse{Message: scm.Response()}
	return true, scm, scr, nil

}

// The docHistory should be in the same format as expected by db.PutExistingRevWithBody(), or empty if this is the first revision
func (bt *BlipTester) SendRevWithHistory(docId, docRev string, revHistory []string, body []byte, properties blip.Properties) (sent bool, req, res *blip.Message, err error) {

	revRequest := blip.NewRequest()
	revRequest.SetCompressed(true)
	revRequest.SetProfile("rev")

	revRequest.Properties["id"] = docId
	revRequest.Properties["rev"] = docRev
	revRequest.Properties["deleted"] = "false"
	if len(revHistory) > 0 {
		revRequest.Properties["history"] = strings.Join(revHistory, ",")
	}

	// Override any properties which have been supplied explicitly
	for k, v := range properties {
		revRequest.Properties[k] = v
	}

	revRequest.SetBody(body)
	sent = bt.sender.Send(revRequest)
	if !sent {
		return sent, revRequest, nil, fmt.Errorf("Failed to send revRequest for doc: %v", docId)
	}
	revResponse := revRequest.Response()
	if revResponse.SerialNumber() != revRequest.SerialNumber() {
		return sent, revRequest, revResponse, fmt.Errorf("revResponse.SerialNumber() != revRequest.SerialNumber().  %v != %v", revResponse.SerialNumber(), revRequest.SerialNumber())
	}

	// Make sure no errors.  Just panic for now, but if there are tests that expect errors and want
	// to use SendRev(), this could be returned.
	if errorCode, ok := revResponse.Properties["Error-Code"]; ok {
		body, _ := revResponse.Body()
		return sent, revRequest, revResponse, fmt.Errorf("Unexpected error sending rev: %v\n%s", errorCode, body)
	}

	return sent, revRequest, revResponse, nil

}

func (bt *BlipTester) SendRev(docId, docRev string, body []byte, properties blip.Properties) (sent bool, req, res *blip.Message, err error) {

	return bt.SendRevWithHistory(docId, docRev, []string{}, body, properties)

}

func getChangesHandler(changesFinishedWg, revsFinishedWg *sync.WaitGroup) func(request *blip.Message) {
	return func(request *blip.Message) {
		// Send a response telling the other side we want ALL revisions

		body, err := request.Body()
		if err != nil {
			panic(fmt.Sprintf("Error getting request body: %v", err))
		}

		if string(body) == "null" {
			changesFinishedWg.Done()
			return
		}

		if !request.NoReply() {

			// unmarshal into json array
			changesBatch := [][]interface{}{}

			if err := base.JSONUnmarshal(body, &changesBatch); err != nil {
				panic(fmt.Sprintf("Error unmarshalling changes. Body: %vs.  Error: %v", string(body), err))
			}

			responseVal := [][]interface{}{}
			for _, change := range changesBatch {
				revId := change[2].(string)
				responseVal = append(responseVal, []interface{}{revId})
				revsFinishedWg.Add(1)
			}

			response := request.Response()
			responseValBytes, err := base.JSONMarshal(responseVal)
			log.Printf("responseValBytes: %s", responseValBytes)
			if err != nil {
				panic(fmt.Sprintf("Error marshalling response: %v", err))
			}
			response.SetBody(responseValBytes)

		}
	}
}

// Get a doc at a particular revision from Sync Gateway.
//
// Warning: this can only be called from a single goroutine, given the fact it registers profile handlers.
//
// If that is not found, it will return an empty resultDoc with no errors.
//
// - Call subChanges (continuous=false) endpoint to get all changes from Sync Gateway
// - Respond to each "change" request telling the other side to send the revision
//   - NOTE: this could be made more efficient by only requesting the revision for the docid/revid pair
//     passed in the parameter.
//
// - If the rev handler is called back with the desired docid/revid pair, save that into a variable that will be returned
// - Block until all pending operations are complete
// - Return the resultDoc or an empty resultDoc
func (bt *BlipTester) GetDocAtRev(requestedDocID, requestedDocRev string) (resultDoc RestDocument, err error) {

	docs := map[string]RestDocument{}
	changesFinishedWg := sync.WaitGroup{}
	revsFinishedWg := sync.WaitGroup{}

	defer func() {
		// Clean up all profile handlers that are registered as part of this test
		delete(bt.blipContext.HandlerForProfile, "changes")
		delete(bt.blipContext.HandlerForProfile, "rev")
	}()

	// -------- Changes handler callback --------
	bt.blipContext.HandlerForProfile["changes"] = getChangesHandler(&changesFinishedWg, &revsFinishedWg)

	// -------- Rev handler callback --------
	bt.blipContext.HandlerForProfile["rev"] = func(request *blip.Message) {

		defer revsFinishedWg.Done()
		body, err := request.Body()
		if err != nil {
			panic(fmt.Sprintf("Unexpected err getting request body: %v", err))
		}
		var doc RestDocument
		err = base.JSONUnmarshal(body, &doc)
		if err != nil {
			panic(fmt.Sprintf("Unexpected err: %v", err))
		}
		docId := request.Properties["id"]
		docRev := request.Properties["rev"]
		doc.SetID(docId)
		doc.SetRevID(docRev)
		docs[docId] = doc

		if docId == requestedDocID && docRev == requestedDocRev {
			resultDoc = doc
		}

	}

	// Send subChanges to subscribe to changes, which will cause the "changes" profile handler above to be called back
	changesFinishedWg.Add(1)
	subChangesRequest := blip.NewRequest()
	subChangesRequest.SetProfile("subChanges")
	subChangesRequest.Properties["continuous"] = "false"

	sent := bt.sender.Send(subChangesRequest)
	if !sent {
		panic("Unable to subscribe to changes.")
	}

	changesFinishedWg.Wait()
	revsFinishedWg.Wait()

	return resultDoc, nil

}

type SendRevWithAttachmentInput struct {
	docId            string
	revId            string
	attachmentName   string
	attachmentLength int
	attachmentBody   string
	attachmentDigest string
	history          []string
	body             []byte
}

// Warning: this can only be called from a single goroutine, given the fact it registers profile handlers.
func (bt *BlipTester) SendRevWithAttachment(input SendRevWithAttachmentInput) (sent bool, req, res *blip.Message) {

	defer func() {
		// Clean up all profile handlers that are registered as part of this test
		delete(bt.blipContext.HandlerForProfile, "getAttachment")
	}()

	// Create a doc with an attachment
	myAttachment := db.DocAttachment{
		ContentType: "application/json",
		Digest:      input.attachmentDigest,
		Length:      input.attachmentLength,
		Revpos:      1,
		Stub:        true,
	}

	doc := NewRestDocument()
	if len(input.body) > 0 {
		unmarshalErr := json.Unmarshal(input.body, &doc)
		if unmarshalErr != nil {
			panic(fmt.Sprintf("Error unmarshalling body into restDocument.  Error: %v", unmarshalErr))
		}
	}

	doc.SetAttachments(db.AttachmentMap{
		input.attachmentName: &myAttachment,
	})

	docBody, err := base.JSONMarshal(doc)
	if err != nil {
		panic(fmt.Sprintf("Error marshalling doc.  Error: %v", err))
	}

	getAttachmentWg := sync.WaitGroup{}

	bt.blipContext.HandlerForProfile["getAttachment"] = func(request *blip.Message) {
		defer getAttachmentWg.Done()
		if request.Properties["digest"] != myAttachment.Digest {
			panic(fmt.Sprintf("Unexpected digest.  Got: %v, expected: %v", request.Properties["digest"], myAttachment.Digest))
		}
		response := request.Response()
		response.SetBody([]byte(input.attachmentBody))
	}

	// Push a rev with an attachment.
	getAttachmentWg.Add(1)
	sent, req, res, _ = bt.SendRevWithHistory(
		input.docId,
		input.revId,
		input.history,
		docBody,
		blip.Properties{},
	)

	// Expect a callback to the getAttachment endpoint
	getAttachmentWg.Wait()

	return sent, req, res

}

func (bt *BlipTester) WaitForNumChanges(numChangesExpected int) (changes [][]interface{}) {

	retryWorker := func() (shouldRetry bool, err error, value interface{}) {
		currentChanges := bt.GetChanges()
		if len(currentChanges) >= numChangesExpected {
			return false, nil, currentChanges
		}

		// haven't seen numDocsExpected yet, so wait and retry
		return true, nil, nil

	}

	_, rawChanges := base.RetryLoop(
		"WaitForNumChanges",
		retryWorker,
		base.CreateDoublingSleeperFunc(10, 10),
	)

	changes, _ = rawChanges.([][]interface{})
	return changes

}

// Returns changes in form of [[sequence, docID, revID, deleted], [sequence, docID, revID, deleted]]
// Warning: this can only be called from a single goroutine, given the fact it registers profile handlers.
func (bt *BlipTester) GetChanges() (changes [][]interface{}) {

	defer func() {
		// Clean up all profile handlers that are registered as part of this test
		delete(bt.blipContext.HandlerForProfile, "changes") // a handler for this profile is registered in SubscribeToChanges
	}()

	collectedChanges := [][]interface{}{}
	chanChanges := make(chan *blip.Message)
	bt.SubscribeToChanges(false, chanChanges)

	for changeMsg := range chanChanges {

		body, err := changeMsg.Body()
		if err != nil {
			panic(fmt.Sprintf("Error getting request body: %v", err))
		}

		if string(body) == "null" {
			// the other side indicated that it's done sending changes.
			// this only works (I think) because continuous=false.
			close(chanChanges)
			break
		}

		// unmarshal into json array
		changesBatch := [][]interface{}{}

		if err := base.JSONUnmarshal(body, &changesBatch); err != nil {
			panic(fmt.Sprintf("Error unmarshalling changes. Body: %vs.  Error: %v", string(body), err))
		}

		collectedChanges = append(collectedChanges, changesBatch...)

	}

	return collectedChanges

}

func (bt *BlipTester) WaitForNumDocsViaChanges(numDocsExpected int) (docs map[string]RestDocument, ok bool) {

	retryWorker := func() (shouldRetry bool, err error, value interface{}) {
		fmt.Println("BT WaitForNumDocsViaChanges retry")
		allDocs := bt.PullDocs()
		if len(allDocs) >= numDocsExpected {
			return false, nil, allDocs
		}

		// haven't seen numDocsExpected yet, so wait and retry
		return true, nil, nil

	}

	_, allDocs := base.RetryLoop(
		"WaitForNumDocsViaChanges",
		retryWorker,
		base.CreateDoublingSleeperFunc(20, 10),
	)

	docs, ok = allDocs.(map[string]RestDocument)
	return docs, ok
}

// Get all documents and their attachments via the following steps:
//
// - Invoking one-shot subChanges request
// - Responding to all incoming "changes" requests from peer to request the changed rev, and accumulate rev body
// - Responding to all incoming "rev" requests from peer to get all attachments, and accumulate them
// - Return accumulated docs + attachments to caller
//
// It is basically a pull replication without the checkpointing
// Warning: this can only be called from a single goroutine, given the fact it registers profile handlers.
func (bt *BlipTester) PullDocs() (docs map[string]RestDocument) {

	docs = map[string]RestDocument{}

	// Mutex to avoid write contention on docs while PullDocs is running (as rev messages may be processed concurrently)
	var docsLock sync.Mutex
	changesFinishedWg := sync.WaitGroup{}
	revsFinishedWg := sync.WaitGroup{}

	defer func() {
		// Clean up all profile handlers that are registered as part of this test
		delete(bt.blipContext.HandlerForProfile, "changes")
		delete(bt.blipContext.HandlerForProfile, "rev")
	}()

	// -------- Changes handler callback --------
	// When this test sends subChanges, Sync Gateway will send a changes request that must be handled
	bt.blipContext.HandlerForProfile["changes"] = getChangesHandler(&changesFinishedWg, &revsFinishedWg)

	// -------- Rev handler callback --------
	bt.blipContext.HandlerForProfile["rev"] = func(request *blip.Message) {

		defer revsFinishedWg.Done()
		body, err := request.Body()
		if err != nil {
			panic(fmt.Sprintf("Unexpected err getting request body: %v", err))
		}
		var doc RestDocument
		err = base.JSONUnmarshal(body, &doc)
		if err != nil {
			panic(fmt.Sprintf("Unexpected err: %v", err))
		}
		docId := request.Properties["id"]
		docRev := request.Properties["rev"]
		doc.SetID(docId)
		doc.SetRevID(docRev)

		docsLock.Lock()
		docs[docId] = doc
		docsLock.Unlock()

		attachments, err := doc.GetAttachments()
		if err != nil {
			panic(fmt.Sprintf("Unexpected err: %v", err))
		}

		for _, attachment := range attachments {

			// Get attachments and append to RestDocument
			getAttachmentRequest := blip.NewRequest()
			getAttachmentRequest.SetProfile(db.MessageGetAttachment)
			getAttachmentRequest.Properties[db.GetAttachmentDigest] = attachment.Digest
			if bt.blipContext.ActiveSubprotocol() == db.BlipCBMobileReplicationV3 {
				getAttachmentRequest.Properties[db.GetAttachmentID] = docId
			}
			sent := bt.sender.Send(getAttachmentRequest)
			if !sent {
				panic("Unable to get attachment.")
			}
			getAttachmentResponse := getAttachmentRequest.Response()
			getAttachmentBody, getAttachmentErr := getAttachmentResponse.Body()
			if getAttachmentErr != nil {
				panic(fmt.Sprintf("Unexpected err: %v", err))
			}
			log.Printf("getAttachmentBody: %s", getAttachmentBody)
			attachment.Data = getAttachmentBody
		}

		// Send response to rev request
		if !request.NoReply() {
			response := request.Response()
			response.SetBody([]byte{}) // Empty response to indicate success
		}

	}

	// -------- Norev handler callback --------
	bt.blipContext.HandlerForProfile["norev"] = func(request *blip.Message) {
		// If a norev is received, then don't bother waiting for one of the expected revisions, since it will never come.
		// The norev could be added to the returned docs map, but so far there is no need for that.  The ability
		// to assert on the number of actually received revisions (which norevs won't affect) meets current test requirements.
		defer revsFinishedWg.Done()
	}

	// Send subChanges to subscribe to changes, which will cause the "changes" profile handler above to be called back
	changesFinishedWg.Add(1)
	subChangesRequest := blip.NewRequest()
	subChangesRequest.SetProfile("subChanges")
	subChangesRequest.Properties["continuous"] = "false"

	sent := bt.sender.Send(subChangesRequest)
	if !sent {
		panic("Unable to subscribe to changes.")
	}

	changesFinishedWg.Wait()

	revsFinishedWg.Wait()

	return docs

}

func (bt *BlipTester) SubscribeToChanges(continuous bool, changes chan<- *blip.Message) {

	// When this test sends subChanges, Sync Gateway will send a changes request that must be handled
	bt.blipContext.HandlerForProfile["changes"] = func(request *blip.Message) {

		changes <- request

		if !request.NoReply() {
			// Send an empty response to avoid the Sync: Invalid response to 'changes' message
			response := request.Response()
			emptyResponseVal := []interface{}{}
			emptyResponseValBytes, err := base.JSONMarshal(emptyResponseVal)
			if err != nil {
				panic(fmt.Sprintf("Error marshalling response: %v", err))
			}
			response.SetBody(emptyResponseValBytes)
		}

	}

	// Send subChanges to subscribe to changes, which will cause the "changes" profile handler above to be called back
	subChangesRequest := blip.NewRequest()
	subChangesRequest.SetProfile("subChanges")
	switch continuous {
	case true:
		subChangesRequest.Properties["continuous"] = "true"
	default:
		subChangesRequest.Properties["continuous"] = "false"
	}

	sent := bt.sender.Send(subChangesRequest)
	if !sent {
		panic("Unable to subscribe to changes.")
	}
	subChangesResponse := subChangesRequest.Response()
	if subChangesResponse.SerialNumber() != subChangesRequest.SerialNumber() {
		panic(fmt.Sprintf("subChangesResponse.SerialNumber() != subChangesRequest.SerialNumber().  %v != %v", subChangesResponse.SerialNumber(), subChangesRequest.SerialNumber()))
	}

}

// Helper for comparing BLIP changes received with expected BLIP changes
type ExpectedChange struct {
	docId    string // DocId or "*" for any doc id
	revId    string // RevId or "*" for any rev id
	sequence string // Sequence or "*" for any sequence
	deleted  *bool  // Deleted status or nil for any deleted status
}

func (e ExpectedChange) Equals(change []interface{}) error {

	// TODO: this is commented because it's giving an error: panic: interface conversion: interface {} is float64, not string [recovered].
	// TODO: I think this should be addressed by adding a BlipChange struct stronger typing than a slice of empty interfaces.  TBA.
	// changeSequence := change[0].(string)

	var changeDeleted *bool

	changeDocId := change[1].(string)
	changeRevId := change[2].(string)
	if len(change) > 3 {
		changeDeletedVal := change[3].(bool)
		changeDeleted = &changeDeletedVal
	}

	if e.docId != "*" && changeDocId != e.docId {
		return fmt.Errorf("changeDocId (%s) != expectedChangeDocId (%s)", changeDocId, e.docId)
	}

	if e.revId != "*" && changeRevId != e.revId {
		return fmt.Errorf("changeRevId (%s) != expectedChangeRevId (%s)", changeRevId, e.revId)
	}

	// TODO: commented due to reasons given above
	// if e.sequence != "*" && changeSequence != e.sequence {
	//	return fmt.Errorf("changeSequence (%s) != expectedChangeSequence (%s)", changeSequence, e.sequence)
	// }

	if changeDeleted != nil && e.deleted != nil && *changeDeleted != *e.deleted {
		return fmt.Errorf("changeDeleted (%v) != expectedChangeDeleted (%v)", *changeDeleted, *e.deleted)
	}

	return nil
}

// Model "CouchDB" style REST documents which define the following special fields:
//
// - _id
// - _rev
// - _removed
// - _deleted (not accounted for yet)
// - _attachments
//
// This struct wraps a map and provides convenience methods for getting at the special
// fields with the appropriate types (string in the id/rev case, db.AttachmentMap in the attachments case).
// Currently only used in tests, but if similar functionality needed in primary codebase, could be moved.
type RestDocument map[string]interface{}

func NewRestDocument() *RestDocument {
	emptyBody := make(map[string]interface{})
	restDoc := RestDocument(emptyBody)
	return &restDoc
}

func (d RestDocument) ID() string {
	rawID, hasID := d[db.BodyId]
	if !hasID {
		return ""
	}
	return rawID.(string)

}

func (d RestDocument) SetID(docId string) {
	d[db.BodyId] = docId
}

func (d RestDocument) RevID() string {
	rawRev, hasRev := d[db.BodyRev]
	if !hasRev {
		return ""
	}
	return rawRev.(string)
}

func (d RestDocument) SetRevID(revId string) {
	d[db.BodyRev] = revId
}

func (d RestDocument) SetAttachments(attachments db.AttachmentMap) {
	d[db.BodyAttachments] = attachments
}

func (d RestDocument) GetAttachments() (db.AttachmentMap, error) {

	rawAttachments, hasAttachments := d[db.BodyAttachments]

	// If the map doesn't even have the _attachments key, return an empty attachments map
	if !hasAttachments {
		return db.AttachmentMap{}, nil
	}

	// Otherwise, create an AttachmentMap from the value in the raw map
	attachmentMap := db.AttachmentMap{}
	switch v := rawAttachments.(type) {
	case db.AttachmentMap:
		// If it's already an AttachmentMap (maybe due to previous call to SetAttachments), then return as-is
		return v, nil
	default:
		rawAttachmentsMap := v.(map[string]interface{})
		for attachmentName, attachmentVal := range rawAttachmentsMap {

			// marshal attachmentVal into a byte array, then unmarshal into a DocAttachment
			attachmentValMarshalled, err := base.JSONMarshal(attachmentVal)
			if err != nil {
				return db.AttachmentMap{}, err
			}
			docAttachment := db.DocAttachment{}
			if err := base.JSONUnmarshal(attachmentValMarshalled, &docAttachment); err != nil {
				return db.AttachmentMap{}, err
			}

			attachmentMap[attachmentName] = &docAttachment
		}

		// Avoid the unnecessary re-Marshal + re-Unmarshal
		d.SetAttachments(attachmentMap)
	}

	return attachmentMap, nil

}

func (d RestDocument) IsRemoved() bool {
	removed, ok := d[db.BodyRemoved]
	if !ok {
		return false
	}
	return removed.(bool)
}

// Wait for the WaitGroup, or return an error if the wg.Wait() doesn't return within timeout
func WaitWithTimeout(wg *sync.WaitGroup, timeout time.Duration) error {

	// Create a channel so that a goroutine waiting on the waitgroup can send it's result (if any)
	wgFinished := make(chan bool)

	go func() {
		wg.Wait()
		wgFinished <- true
	}()

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-wgFinished:
		return nil
	case <-timer.C:
		return fmt.Errorf("Timed out waiting after %v", timeout)
	}

}

// NewHTTPTestServerOnListener returns a new httptest server, which is configured to listen on the given listener.
// This is useful when you need to know the listen address before you start up a server.
func NewHTTPTestServerOnListener(h http.Handler, l net.Listener) *httptest.Server {
	s := &httptest.Server{
		Config:   &http.Server{Handler: h},
		Listener: l,
	}
	s.Start()
	return s
}

func waitAndRequireCondition(t *testing.T, fn func() bool, failureMsgAndArgs ...interface{}) {
	t.Log("starting waitAndRequireCondition")
	for i := 0; i <= 20; i++ {
		if i == 20 {
			require.Fail(t, "Condition failed to be satisfied", failureMsgAndArgs...)
		}
		if fn() {
			break
		}
		time.Sleep(time.Millisecond * 250)
	}
}

func WaitAndAssertCondition(t *testing.T, fn func() bool, failureMsgAndArgs ...interface{}) {
	t.Log("starting WaitAndAssertCondition")
	for i := 0; i <= 20; i++ {
		if i == 20 {
			assert.Fail(t, "Condition failed to be satisfied", failureMsgAndArgs...)
		}
		if fn() {
			break
		}
		time.Sleep(time.Millisecond * 250)
	}
}

func WaitAndAssertConditionTimeout(t *testing.T, timeout time.Duration, fn func() bool, failureMsgAndArgs ...interface{}) {
	start := time.Now()
	tick := time.NewTicker(timeout / 20)
	defer tick.Stop()
	for range tick.C {
		if time.Since(start) > timeout {
			assert.Fail(t, "Condition failed to be satisfied", failureMsgAndArgs...)
		}
		if fn() {
			return
		}
	}
}

func WaitAndAssertBackgroundManagerState(t testing.TB, expected db.BackgroundProcessState, getStateFunc func(t testing.TB) db.BackgroundProcessState) bool {
	err, actual := base.RetryLoop(t.Name()+"-WaitAndAssertBackgroundManagerState", func() (shouldRetry bool, err error, value interface{}) {
		actual := getStateFunc(t)
		return expected != actual, nil, actual
	}, base.CreateMaxDoublingSleeperFunc(30, 100, 1000))
	return assert.NoErrorf(t, err, "expected background manager state %v, but got: %v", expected, actual)
}

func WaitAndAssertBackgroundManagerExpiredHeartbeat(t testing.TB, bm *db.BackgroundManager) bool {
	err, b := base.RetryLoop(t.Name()+"-assertNoHeartbeatDoc", func() (shouldRetry bool, err error, value interface{}) {
		b, err := bm.GetHeartbeatDoc(t)
		return !base.IsDocNotFoundError(err), err, b
	}, base.CreateMaxDoublingSleeperFunc(30, 100, 1000))
	if b != nil {
		return assert.NoErrorf(t, err, "expected heartbeat doc to expire, but found one: %v", b)
	}
	return assert.Truef(t, base.IsDocNotFoundError(err), "expected heartbeat doc to expire, but got a different error: %v", err)
}

// RespRevID returns a rev ID from the given response, or fails the given test if a rev ID was not found.
func RespRevID(t *testing.T, response *TestResponse) (revID string) {
	var r struct {
		RevID *string `json:"rev"`
	}
	require.NoError(t, json.Unmarshal(response.BodyBytes(), &r), "couldn't decode JSON from response body")
	require.NotNil(t, r.RevID, "expecting non-nil rev ID from response: %s", string(response.BodyBytes()))
	require.NotEqual(t, "", *r.RevID, "expecting non-empty rev ID from response: %s", string(response.BodyBytes()))
	return *r.RevID
}

func MarshalConfig(t *testing.T, config db.ReplicationConfig) string {
	replicationPayload, err := json.Marshal(config)
	require.NoError(t, err)
	return string(replicationPayload)
}

func HasActiveChannel(channelSet map[string]interface{}, channelName string) bool {
	if channelSet == nil {
		return false
	}
	value, ok := channelSet[channelName]
	if !ok || value != nil { // An entry for the channel name with a nil value represents an active channel
		return false
	}

	return true
}

func (sc *ServerContext) isDatabaseSuspended(t *testing.T, dbName string) bool {
	sc.lock.RLock()
	defer sc.lock.RUnlock()
	return sc._isDatabaseSuspended(dbName)
}

func (sc *ServerContext) suspendDatabase(t *testing.T, ctx context.Context, dbName string) error {
	sc.lock.Lock()
	defer sc.lock.Unlock()

	return sc._suspendDatabase(ctx, dbName)
}
