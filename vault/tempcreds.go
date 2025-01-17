package vault

import (
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/99designs/keyring"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/sts"
)

const DefaultExpirationWindow = 5 * time.Minute

func newSession(creds *credentials.Credentials, region string) *session.Session {
	return session.Must(session.NewSession(aws.NewConfig().WithRegion(region).WithCredentials(creds)))
}

func newStsClient(creds *credentials.Credentials, region string) *sts.STS {
	return sts.New(newSession(creds, region))
}

// NewTempCredentials creates temporary credentials
func NewTempCredentials(k keyring.Keyring, config *Config) (*credentials.Credentials, error) {
	provider, err := NewTempCredentialsProvider(k, config)
	if err != nil {
		return nil, err
	}

	return credentials.NewCredentials(provider), nil
}

// NewTempCredentials creates a provider for temporary credentials
func NewTempCredentialsProvider(k keyring.Keyring, config *Config) (*TempCredentialsProvider, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}

	return &TempCredentialsProvider{
		masterCreds: NewMasterCredentials(k, config.CredentialsName),
		config:      config,
		sessions:    &KeyringSessions{k},
	}, nil
}

// TempCredentialsProvider provides credentials protected by GetSessionToken and AssumeRole where possible
type TempCredentialsProvider struct {
	credentials.Expiry
	masterCreds         *credentials.Credentials
	sessions            *KeyringSessions
	config              *Config
	forceSessionRefresh bool
}

func (p *TempCredentialsProvider) ForceRefresh() {
	p.masterCreds.Expire()
	p.forceSessionRefresh = true
}

func (p *TempCredentialsProvider) Retrieve() (credentials.Value, error) {
	if p.config.NoSession && p.config.RoleARN == "" {
		log.Println("Using master credentials")
		return p.masterCreds.Get()
	}
	if p.config.NoSession {
		return p.getCredsWithRole()
	}
	if p.config.RoleARN == "" {
		return p.getCredsWithSession()
	}

	return p.getCredsWithSessionAndRole()
}

func (p *TempCredentialsProvider) getCredsWithSession() (credentials.Value, error) {
	log.Println("Getting credentials with GetSessionToken")

	session, err := p.getSessionToken()
	if err != nil {
		return credentials.Value{}, nil
	}

	p.SetExpiration(*session.Expiration, DefaultExpirationWindow)

	value := credentials.Value{
		AccessKeyID:     *session.AccessKeyId,
		SecretAccessKey: *session.SecretAccessKey,
		SessionToken:    *session.SessionToken,
	}

	log.Printf("Using session token ****************%s, expires in %s", (*session.AccessKeyId)[len(*session.AccessKeyId)-4:], session.Expiration.Sub(time.Now()).String())
	return value, nil
}

func (p *TempCredentialsProvider) getCredsWithSessionAndRole() (credentials.Value, error) {
	log.Println("Getting credentials with GetSessionToken and AssumeRole")

	session, err := p.getSessionToken()
	if err != nil {
		return credentials.Value{}, nil
	}

	role, err := p.assumeRoleFromSession(session)
	if err != nil {
		return credentials.Value{}, err
	}

	p.SetExpiration(*role.Expiration, DefaultExpirationWindow)

	creds := credentials.Value{
		AccessKeyID:     *role.AccessKeyId,
		SecretAccessKey: *role.SecretAccessKey,
		SessionToken:    *role.SessionToken,
	}

	log.Printf("Using session token ****************%s with role ****************%s, expires in %s",
		(*session.AccessKeyId)[len(*session.AccessKeyId)-4:],
		(*role.AccessKeyId)[len(*role.AccessKeyId)-4:],
		role.Expiration.Sub(time.Now()).String())

	return creds, nil
}

// getCredsWithRole returns credentials a session created with AssumeRole
func (p *TempCredentialsProvider) getCredsWithRole() (credentials.Value, error) {
	log.Println("Getting credentials with AssumeRole")

	if p.config.RoleARN == "" {
		return credentials.Value{}, errors.New("No role defined")
	}

	creds, err := p.masterCreds.Get()
	if err != nil {
		return credentials.Value{}, err
	}

	role, err := p.assumeRoleFromCreds(creds)
	if err != nil {
		return credentials.Value{}, err
	}

	p.SetExpiration(*role.Expiration, DefaultExpirationWindow)

	log.Printf("Using role ****************%s, expires in %s", (*role.AccessKeyId)[len(*role.AccessKeyId)-4:], role.Expiration.Sub(time.Now()).String())
	return credentials.Value{
		AccessKeyID:     *role.AccessKeyId,
		SecretAccessKey: *role.SecretAccessKey,
		SessionToken:    *role.SessionToken,
	}, nil
}

func (p *TempCredentialsProvider) createSessionToken() (*sts.Credentials, error) {
	log.Printf("Creating new session token for profile %s", p.config.CredentialsName)

	params := &sts.GetSessionTokenInput{
		DurationSeconds: aws.Int64(int64(p.config.SessionDuration.Seconds())),
	}

	if p.config.MfaSerial != "" {
		params.SerialNumber = aws.String(p.config.MfaSerial)
		if p.config.MfaToken == "" {
			token, err := p.config.MfaPrompt(fmt.Sprintf("Enter token for %s: ", p.config.MfaSerial))
			if err != nil {
				return nil, err
			}
			params.TokenCode = aws.String(token)
		} else {
			params.TokenCode = aws.String(p.config.MfaToken)
		}
	}

	client := newStsClient(p.masterCreds, p.config.Region)

	resp, err := client.GetSessionToken(params)
	if err != nil {
		return nil, err
	}

	return resp.Credentials, nil
}

func (p *TempCredentialsProvider) getSessionToken() (*sts.Credentials, error) {
	if p.forceSessionRefresh {
		return p.createSessionToken()
	}

	session, err := p.sessions.Retrieve(p.config.CredentialsName, p.config.MfaSerial)
	if err != nil {
		// session lookup missed, we need to create a new one.
		session, err = p.createSessionToken()
		if err != nil {
			return nil, err
		}

		err = p.sessions.Store(p.config.CredentialsName, p.config.MfaSerial, session)
		if err != nil {
			return nil, err
		}
	}

	return session, err
}

func (p *TempCredentialsProvider) roleSessionName() string {
	if p.config.RoleSessionName != "" {
		return p.config.RoleSessionName
	}

	// Try to work out a role name that will hopefully end up unique.
	return fmt.Sprintf("%d", time.Now().UTC().UnixNano())
}

// assumeRoleFromSession takes a session created with GetSessionToken and uses that to assume a role
func (p *TempCredentialsProvider) assumeRoleFromSession(session *sts.Credentials) (sts.Credentials, error) {
	client := newStsClient(credentials.NewStaticCredentials(*session.AccessKeyId, *session.SecretAccessKey, *session.SessionToken), p.config.Region)

	input := &sts.AssumeRoleInput{
		RoleArn:         aws.String(p.config.RoleARN),
		RoleSessionName: aws.String(p.roleSessionName()),
		DurationSeconds: aws.Int64(int64(p.config.AssumeRoleDuration.Seconds())),
	}

	if p.config.ExternalID != "" {
		input.ExternalId = aws.String(p.config.ExternalID)
	}

	log.Printf("Assuming role %s from session token", p.config.RoleARN)
	resp, err := client.AssumeRole(input)
	if err != nil {
		return sts.Credentials{}, err
	}

	return *resp.Credentials, nil
}

// assumeRoleFromCreds uses IAM credentials to assume a role
func (p *TempCredentialsProvider) assumeRoleFromCreds(creds credentials.Value) (sts.Credentials, error) {
	if p.config.RoleARN == "" {
		return sts.Credentials{}, errors.New("No role defined")
	}

	client := newStsClient(credentials.NewStaticCredentialsFromCreds(creds), p.config.Region)

	input := &sts.AssumeRoleInput{
		RoleArn:         aws.String(p.config.RoleARN),
		RoleSessionName: aws.String(p.roleSessionName()),
		DurationSeconds: aws.Int64(int64(p.config.AssumeRoleDuration.Seconds())),
	}

	if p.config.ExternalID != "" {
		input.ExternalId = aws.String(p.config.ExternalID)
	}

	// if we don't have a session, we need to include MFA token in the AssumeRole call
	if p.config.MfaSerial != "" {
		input.SerialNumber = aws.String(p.config.MfaSerial)
		if p.config.MfaToken == "" {
			token, err := p.config.MfaPrompt(fmt.Sprintf("Enter token for %s: ", p.config.MfaSerial))
			if err != nil {
				return sts.Credentials{}, err
			}
			input.TokenCode = aws.String(token)
		} else {
			input.TokenCode = aws.String(p.config.MfaToken)
		}
	}

	log.Printf("Assuming role %s with iam credentials", p.config.RoleARN)
	resp, err := client.AssumeRole(input)
	if err != nil {
		return sts.Credentials{}, err
	}

	return *resp.Credentials, nil
}
