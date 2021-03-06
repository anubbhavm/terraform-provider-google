package google

import (
	"context"
	"fmt"
	"log"
	"os"
	"testing"
	"time"

	"google.golang.org/api/cloudkms/v1"
	cloudresourcemanager "google.golang.org/api/cloudresourcemanager/v1"
	"google.golang.org/api/iam/v1"
)

var SharedKeyRing = "tftest-shared-keyring-1"
var SharedCryptoKey = map[string]string{
	"ENCRYPT_DECRYPT":    "tftest-shared-key-1",
	"ASYMMETRIC_SIGN":    "tftest-shared-sign-key-1",
	"ASYMMETRIC_DECRYPT": "tftest-shared-decrypt-key-1",
}

type bootstrappedKMS struct {
	*cloudkms.KeyRing
	*cloudkms.CryptoKey
}

func BootstrapKMSKey(t *testing.T) bootstrappedKMS {
	return BootstrapKMSKeyInLocation(t, "global")
}

func BootstrapKMSKeyInLocation(t *testing.T, locationID string) bootstrappedKMS {
	return BootstrapKMSKeyWithPurposeInLocation(t, "ENCRYPT_DECRYPT", locationID)
}

// BootstrapKMSKeyWithPurpose returns a KMS key in the "global" location.
// See BootstrapKMSKeyWithPurposeInLocation.
func BootstrapKMSKeyWithPurpose(t *testing.T, purpose string) bootstrappedKMS {
	return BootstrapKMSKeyWithPurposeInLocation(t, purpose, "global")
}

/**
* BootstrapKMSKeyWithPurposeInLocation will return a KMS key in a
* particular location with the given purpose that can be used
* in tests that are testing KMS integration with other resources.
*
* This will either return an existing key or create one if it hasn't been created
* in the project yet. The motivation is because keyrings don't get deleted and we
* don't want a linear growth of disabled keyrings in a project. We also don't want
* to incur the overhead of creating a new project for each test that needs to use
* a KMS key.
**/
func BootstrapKMSKeyWithPurposeInLocation(t *testing.T, purpose, locationID string) bootstrappedKMS {
	return BootstrapKMSKeyWithPurposeInLocationAndName(t, purpose, locationID, SharedCryptoKey[purpose])
}

func BootstrapKMSKeyWithPurposeInLocationAndName(t *testing.T, purpose, locationID, keyShortName string) bootstrappedKMS {
	config := BootstrapConfig(t)
	if config == nil {
		return bootstrappedKMS{
			&cloudkms.KeyRing{},
			&cloudkms.CryptoKey{},
		}
	}

	projectID := getTestProjectFromEnv()
	keyRingParent := fmt.Sprintf("projects/%s/locations/%s", projectID, locationID)
	keyRingName := fmt.Sprintf("%s/keyRings/%s", keyRingParent, SharedKeyRing)
	keyParent := fmt.Sprintf("projects/%s/locations/%s/keyRings/%s", projectID, locationID, SharedKeyRing)
	keyName := fmt.Sprintf("%s/cryptoKeys/%s", keyParent, keyShortName)

	// Get or Create the hard coded shared keyring for testing
	kmsClient := config.clientKms
	keyRing, err := kmsClient.Projects.Locations.KeyRings.Get(keyRingName).Do()
	if err != nil {
		if isGoogleApiErrorWithCode(err, 404) {
			keyRing, err = kmsClient.Projects.Locations.KeyRings.Create(keyRingParent, &cloudkms.KeyRing{}).
				KeyRingId(SharedKeyRing).Do()
			if err != nil {
				t.Errorf("Unable to bootstrap KMS key. Cannot create keyRing: %s", err)
			}
		} else {
			t.Errorf("Unable to bootstrap KMS key. Cannot retrieve keyRing: %s", err)
		}
	}

	if keyRing == nil {
		t.Fatalf("Unable to bootstrap KMS key. keyRing is nil!")
	}

	// Get or Create the hard coded, shared crypto key for testing
	cryptoKey, err := kmsClient.Projects.Locations.KeyRings.CryptoKeys.Get(keyName).Do()
	if err != nil {
		if isGoogleApiErrorWithCode(err, 404) {
			algos := map[string]string{
				"ENCRYPT_DECRYPT":    "GOOGLE_SYMMETRIC_ENCRYPTION",
				"ASYMMETRIC_SIGN":    "RSA_SIGN_PKCS1_4096_SHA512",
				"ASYMMETRIC_DECRYPT": "RSA_DECRYPT_OAEP_4096_SHA512",
			}
			template := cloudkms.CryptoKeyVersionTemplate{
				Algorithm: algos[purpose],
			}

			newKey := cloudkms.CryptoKey{
				Purpose:         purpose,
				VersionTemplate: &template,
			}

			cryptoKey, err = kmsClient.Projects.Locations.KeyRings.CryptoKeys.Create(keyParent, &newKey).
				CryptoKeyId(keyShortName).Do()
			if err != nil {
				t.Errorf("Unable to bootstrap KMS key. Cannot create new CryptoKey: %s", err)
			}

		} else {
			t.Errorf("Unable to bootstrap KMS key. Cannot call CryptoKey service: %s", err)
		}
	}

	if cryptoKey == nil {
		t.Fatalf("Unable to bootstrap KMS key. CryptoKey is nil!")
	}

	return bootstrappedKMS{
		keyRing,
		cryptoKey,
	}
}

var serviceAccountEmail = "tf-bootstrap-service-account"
var serviceAccountDisplay = "Bootstrapped Service Account for Terraform tests"

// Some tests need a second service account, other than the test runner, to assert functionality on.
// This provides a well-known service account that can be used when dynamically creating a service
// account isn't an option.
func getOrCreateServiceAccount(config *Config, project string) (*iam.ServiceAccount, error) {
	name := fmt.Sprintf("projects/%s/serviceAccounts/%s@%s.iam.gserviceaccount.com", project, serviceAccountEmail, project)
	log.Printf("[DEBUG] Verifying %s as bootstrapped service account.\n", name)

	sa, err := config.clientIAM.Projects.ServiceAccounts.Get(name).Do()
	if err != nil && !isGoogleApiErrorWithCode(err, 404) {
		return nil, err
	}

	if sa == nil {
		log.Printf("[DEBUG] Account missing. Creating %s as bootstrapped service account.\n", name)
		sa = &iam.ServiceAccount{
			DisplayName: serviceAccountDisplay,
		}

		r := &iam.CreateServiceAccountRequest{
			AccountId:      serviceAccountEmail,
			ServiceAccount: sa,
		}
		sa, err = config.clientIAM.Projects.ServiceAccounts.Create("projects/"+project, r).Do()
		if err != nil {
			return nil, err
		}
	}

	return sa, nil
}

// In order to test impersonation we need to grant the testRunner's account the ability to grant tokens
// on a different service account. Granting permissions takes time and there is no operation to wait on
// so instead this creates a single service account once per test-suite with the correct permissions.
// The first time this test is run it will fail, but subsequent runs will succeed.
func impersonationServiceAccountPermissions(config *Config, sa *iam.ServiceAccount, testRunner string) error {
	log.Printf("[DEBUG] Setting service account permissions.\n")
	policy := iam.Policy{
		Bindings: []*iam.Binding{},
	}

	binding := &iam.Binding{
		Role:    "roles/iam.serviceAccountTokenCreator",
		Members: []string{"serviceAccount:" + sa.Email, "serviceAccount:" + testRunner},
	}
	policy.Bindings = append(policy.Bindings, binding)

	// Overwrite the roles each time on this service account. This is because this account is
	// only created for the test suite and will stop snowflaking of permissions to get tests
	// to run. Overwriting permissions on 1 service account shouldn't affect others.
	_, err := config.clientIAM.Projects.ServiceAccounts.SetIamPolicy(sa.Name, &iam.SetIamPolicyRequest{
		Policy: &policy,
	}).Do()
	if err != nil {
		return err
	}

	return nil
}

func BootstrapServiceAccount(t *testing.T, project, testRunner string) string {
	config := BootstrapConfig(t)
	if config == nil {
		return ""
	}

	sa, err := getOrCreateServiceAccount(config, project)
	if err != nil {
		t.Fatalf("Bootstrapping failed. Cannot retrieve service account, %s", err)
	}

	err = impersonationServiceAccountPermissions(config, sa, testRunner)
	if err != nil {
		t.Fatalf("Bootstrapping failed. Cannot set service account permissions, %s", err)
	}

	return sa.Email
}

const SharedTestNetworkPrefix = "tf-bootstrap-net-"

// BootstrapSharedTestNetwork will return a shared compute network
// for a test or set of tests. Often resources create complementing
// tenant network resources, which we don't control and which don't get cleaned
// up after our owned resource is deleted in test. These tenant resources
// have quotas, so creating a shared test network prevents hitting these limits.
//
// testId specifies the test/suite for which a shared network is used/initialized.
// Returns the name of an network, creating it if hasn't been created in the test projcet.
func BootstrapSharedTestNetwork(t *testing.T, testId string) string {
	project := getTestProjectFromEnv()
	networkName := SharedTestNetworkPrefix + testId

	config := BootstrapConfig(t)
	if config == nil {
		return ""
	}

	log.Printf("[DEBUG] Getting shared test network %q", networkName)
	_, err := config.NewComputeClient(config.userAgent).Networks.Get(project, networkName).Do()
	if err != nil && isGoogleApiErrorWithCode(err, 404) {
		log.Printf("[DEBUG] Network %q not found, bootstrapping", networkName)
		url := fmt.Sprintf("%sprojects/%s/global/networks", config.ComputeBasePath, project)
		netObj := map[string]interface{}{
			"name":                  networkName,
			"autoCreateSubnetworks": false,
		}

		res, err := sendRequestWithTimeout(config, "POST", project, url, config.userAgent, netObj, 4*time.Minute)
		if err != nil {
			t.Fatalf("Error bootstrapping shared test network %q: %s", networkName, err)
		}

		log.Printf("[DEBUG] Waiting for network creation to finish")
		err = computeOperationWaitTime(config, res, project, "Error bootstrapping shared test network", config.userAgent, 4*time.Minute)
		if err != nil {
			t.Fatalf("Error bootstrapping shared test network %q: %s", networkName, err)
		}
	}

	network, err := config.NewComputeClient(config.userAgent).Networks.Get(project, networkName).Do()
	if err != nil {
		t.Errorf("Error getting shared test network %q: %s", networkName, err)
	}
	if network == nil {
		t.Fatalf("Error getting shared test network %q: is nil", networkName)
	}
	return network.Name
}

var SharedServicePerimeterProjectPrefix = "tf-bootstrap-sp-"

func BootstrapServicePerimeterProjects(t *testing.T, desiredProjects int) []*cloudresourcemanager.Project {
	config := BootstrapConfig(t)
	if config == nil {
		return nil
	}

	org := getTestOrgFromEnv(t)

	// The filter endpoint works differently if you provide both the parent id and parent type, and
	// doesn't seem to allow for prefix matching. Don't change this to include the parent type unless
	// that API behavior changes.
	prefixFilter := fmt.Sprintf("id:%s* parent.id:%s", SharedServicePerimeterProjectPrefix, org)
	res, err := config.clientResourceManager.Projects.List().Filter(prefixFilter).Do()
	if err != nil {
		t.Fatalf("Error getting shared test projects: %s", err)
	}

	projects := res.Projects
	for len(projects) < desiredProjects {
		pid := SharedServicePerimeterProjectPrefix + randString(t, 10)
		project := &cloudresourcemanager.Project{
			ProjectId: pid,
			Name:      "TF Service Perimeter Test",
			Parent: &cloudresourcemanager.ResourceId{
				Type: "organization",
				Id:   org,
			},
		}
		op, err := config.clientResourceManager.Projects.Create(project).Do()
		if err != nil {
			t.Fatalf("Error bootstrapping shared test project: %s", err)
		}

		opAsMap, err := ConvertToMap(op)
		if err != nil {
			t.Fatalf("Error bootstrapping shared test project: %s", err)
		}

		err = resourceManagerOperationWaitTime(config, opAsMap, "creating project", config.userAgent, 4)
		if err != nil {
			t.Fatalf("Error bootstrapping shared test project: %s", err)
		}

		p, err := config.clientResourceManager.Projects.Get(pid).Do()
		if err != nil {
			t.Fatalf("Error getting shared test project: %s", err)
		}
		projects = append(projects, p)
	}

	return projects
}

func BootstrapConfig(t *testing.T) *Config {
	if v := os.Getenv("TF_ACC"); v == "" {
		t.Skip("Acceptance tests and bootstrapping skipped unless env 'TF_ACC' set")
		return nil
	}

	config := &Config{
		Credentials: getTestCredsFromEnv(),
		Project:     getTestProjectFromEnv(),
		Region:      getTestRegionFromEnv(),
		Zone:        getTestZoneFromEnv(),
	}

	ConfigureBasePaths(config)

	if err := config.LoadAndValidate(context.Background()); err != nil {
		t.Fatalf("Bootstrapping failed. Unable to load test config: %s", err)
	}
	return config
}
