package ssh

import (
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/hashicorp/vault/helper/uuid"
	"github.com/hashicorp/vault/logical"
	"github.com/hashicorp/vault/logical/framework"
)

type sshOTP struct {
	Username string `json:"username"`
	IP       string `json:"ip"`
}

func pathCredsCreate(b *backend) *framework.Path {
	return &framework.Path{
		Pattern: "creds/" + framework.GenericNameRegex("role"),
		Fields: map[string]*framework.FieldSchema{
			"role": &framework.FieldSchema{
				Type:        framework.TypeString,
				Description: "[Required] Name of the role",
			},
			"username": &framework.FieldSchema{
				Type:        framework.TypeString,
				Description: "[Optional] Username in remote host",
			},
			"ip": &framework.FieldSchema{
				Type:        framework.TypeString,
				Description: "[Required] IP of the remote host",
			},
		},
		Callbacks: map[logical.Operation]framework.OperationFunc{
			logical.WriteOperation: b.pathCredsCreateWrite,
		},
		HelpSynopsis:    pathCredsCreateHelpSyn,
		HelpDescription: pathCredsCreateHelpDesc,
	}
}

func (b *backend) pathCredsCreateWrite(
	req *logical.Request, d *framework.FieldData) (*logical.Response, error) {
	roleName := d.Get("role").(string)
	if roleName == "" {
		return logical.ErrorResponse("Missing role"), nil
	}

	ipRaw := d.Get("ip").(string)
	if ipRaw == "" {
		return logical.ErrorResponse("Missing ip"), nil
	}

	role, err := b.getRole(req.Storage, roleName)
	if err != nil {
		return nil, fmt.Errorf("error retrieving role: %s", err)
	}
	if role == nil {
		return logical.ErrorResponse(fmt.Sprintf("Role '%s' not found", roleName)), nil
	}

	// username is an optional parameter.
	username := d.Get("username").(string)

	// Set the default username
	if username == "" {
		if role.DefaultUser == "" {
			return logical.ErrorResponse("No default username registered. Use 'username' option"), nil
		}
		username = role.DefaultUser
	}

	if role.AllowedUsers != "" {
		// Check if the username is present in allowed users list.
		err := validateUsername(username, role.AllowedUsers)

		// If username is not present in allowed users list, check if it
		// is the default username in the role. If neither is true, then
		// that username is not allowed to generate a credential.
		if err != nil && username != role.DefaultUser {
			return logical.ErrorResponse("Username is not present is allowed users list."), nil
		}
	}

	// Validate the IP address
	ipAddr := net.ParseIP(ipRaw)
	if ipAddr == nil {
		return logical.ErrorResponse(fmt.Sprintf("Invalid IP '%s'", ipRaw)), nil
	}

	// Check if the IP belongs to the registered list of CIDR blocks under the role
	ip := ipAddr.String()
	err = validateIP(ip, role.CIDRList, role.ExcludeCIDRList)
	if err != nil {
		return logical.ErrorResponse(fmt.Sprintf("Error validating IP: %s", err)), nil
	}

	var result *logical.Response
	if role.KeyType == KeyTypeOTP {
		// Generate an OTP
		otp, err := b.GenerateOTPCredential(req, username, ip)
		if err != nil {
			return nil, err
		}

		// Return the information relevant to user of OTP type and save
		// the data required for later use in the internal section of secret.
		// In this case, saving just the OTP is sufficient since there is
		// no need to establish connection with the remote host.
		result = b.Secret(SecretOTPType).Response(map[string]interface{}{
			"key_type": role.KeyType,
			"key":      otp,
			"username": username,
			"ip":       ip,
			"port":     role.Port,
		}, map[string]interface{}{
			"otp": otp,
		})
	} else if role.KeyType == KeyTypeDynamic {
		// Generate an RSA key pair. This also installs the newly generated
		// public key in the remote host.
		dynamicPublicKey, dynamicPrivateKey, err := b.GenerateDynamicCredential(req, role, username, ip)
		if err != nil {
			return nil, err
		}

		// Return the information relevant to user of dynamic type and save
		// information required for later use in internal section of secret.
		result = b.Secret(SecretDynamicKeyType).Response(map[string]interface{}{
			"key":      dynamicPrivateKey,
			"key_type": role.KeyType,
			"username": username,
			"ip":       ip,
			"port":     role.Port,
		}, map[string]interface{}{
			"admin_user":         role.AdminUser,
			"username":           username,
			"ip":                 ip,
			"host_key_name":      role.KeyName,
			"dynamic_public_key": dynamicPublicKey,
			"port":               role.Port,
			"install_script":     role.InstallScript,
		})
	} else {
		return nil, fmt.Errorf("key type unknown")
	}

	// Change the lease information to reflect user's choice
	lease, _ := b.Lease(req.Storage)

	// If the lease information is set, update it in secret.
	if lease != nil {
		result.Secret.TTL = lease.Lease
		result.Secret.GracePeriod = lease.LeaseMax
	}

	// If lease information is not set, set it to 10 minutes.
	if lease == nil {
		result.Secret.TTL = 10 * time.Minute
		result.Secret.GracePeriod = 2 * time.Minute
	}

	return result, nil
}

// Generates a RSA key pair and installs it in the remote target
func (b *backend) GenerateDynamicCredential(req *logical.Request, role *sshRole, username, ip string) (string, string, error) {
	// Fetch the host key to be used for dynamic key installation
	keyEntry, err := req.Storage.Get(fmt.Sprintf("keys/%s", role.KeyName))
	if err != nil {
		return "", "", fmt.Errorf("key '%s' not found. err:%s", role.KeyName, err)
	}

	if keyEntry == nil {
		return "", "", fmt.Errorf("key '%s' not found", role.KeyName)
	}

	var hostKey sshHostKey
	if err := keyEntry.DecodeJSON(&hostKey); err != nil {
		return "", "", fmt.Errorf("error reading the host key: %s", err)
	}

	// Generate a new RSA key pair with the given key length.
	dynamicPublicKey, dynamicPrivateKey, err := generateRSAKeys(role.KeyBits)
	if err != nil {
		return "", "", fmt.Errorf("error generating key: %s", err)
	}

	if len(role.KeyOptionSpecs) != 0 {
		dynamicPublicKey = fmt.Sprintf("%s %s", role.KeyOptionSpecs, dynamicPublicKey)
	}

	// Add the public key to authorized_keys file in target machine
	err = b.installPublicKeyInTarget(role.AdminUser, username, ip, role.Port, hostKey.Key, dynamicPublicKey, role.InstallScript, true)
	if err != nil {
		return "", "", fmt.Errorf("error adding public key to authorized_keys file in target")
	}
	return dynamicPublicKey, dynamicPrivateKey, nil
}

// Generates a UUID OTP and its salted value based on the salt of the backend.
func (b *backend) GenerateSaltedOTP() (string, string) {
	str := uuid.GenerateUUID()
	return str, b.salt.SaltID(str)
}

// Generates an UUID OTP and creates an entry for the same in storage backend with its salted string.
func (b *backend) GenerateOTPCredential(req *logical.Request, username, ip string) (string, error) {
	otp, otpSalted := b.GenerateSaltedOTP()

	// Check if there is an entry already created for the newly generated OTP.
	entry, err := b.getOTP(req.Storage, otpSalted)

	// If entry already exists for the OTP, make sure that new OTP is not
	// replacing an existing one by recreating new ones until an unused
	// OTP is generated. It is very unlikely that this is the case and this
	// code is just for safety.
	for err == nil && entry != nil {
		otp, otpSalted = b.GenerateSaltedOTP()
		entry, err = b.getOTP(req.Storage, otpSalted)
		if err != nil {
			return "", err
		}
	}

	// Store an entry for the salt of OTP.
	newEntry, err := logical.StorageEntryJSON("otp/"+otpSalted, sshOTP{
		Username: username,
		IP:       ip,
	})
	if err != nil {
		return "", err
	}
	if err := req.Storage.Put(newEntry); err != nil {
		return "", err
	}
	return otp, nil
}

// Validates the IP address by first searching the IP in the allowed CIDR
// blocks registered with the role. If there is found, then it is searched
// in the excluded CIDR blocks and if there is a match there, an error is
// returned. IP is valid only if it is encompassed by allowed CIDR blocks
// and not by excluded CIDR blocks.
func validateIP(ip, cidrList, excludeCidrList string) error {
	ipMatched, err := cidrListContainsIP(ip, cidrList)
	if err != nil {
		return err
	}
	if !ipMatched {
		return fmt.Errorf("IP does not belong to role")
	}

	if len(excludeCidrList) == 0 {
		return nil
	}

	ipMatched, err = cidrListContainsIP(ip, excludeCidrList)
	if err != nil {
		return err
	}
	if ipMatched {
		return fmt.Errorf("IP does not belong to role")
	}

	return nil
}

// Checks if the username supplied by the user is present in the list of
// allowed users registered which creation of role.
func validateUsername(username, allowedUsers string) error {
	userList := strings.Split(allowedUsers, ",")
	for _, user := range userList {
		if user == username {
			return nil
		}
	}
	return fmt.Errorf("username not in allowed users list")
}

const pathCredsCreateHelpSyn = `
Creates a credential for establishing SSH connection with the remote host.
`

const pathCredsCreateHelpDesc = `
This path will generate a new key for establishing SSH session with
target host. The key can either be a long lived dynamic key or a One
Time Password (OTP), using 'key_type' parameter being 'dynamic' or
'otp' respectively. For dynamic keys, a named key should be supplied.
Create named key using the 'keys/' endpoint, and this represents the
shared SSH key of target host. If this backend is mounted at 'ssh',
then "ssh/creds/web" would generate a key for 'web' role.

Keys will have a lease associated with them. The access keys can be
revoked by using the lease ID.
`
