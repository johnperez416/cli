package verify

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/sigstore/sigstore-go/pkg/fulcio/certificate"
	"github.com/sigstore/sigstore-go/pkg/verify"

	"github.com/cli/cli/v2/pkg/cmd/attestation/api"
	"github.com/cli/cli/v2/pkg/cmd/attestation/artifact"
	"github.com/cli/cli/v2/pkg/cmd/attestation/verification"
)

const (
	// represents the GitHub hosted runner in the certificate RunnerEnvironment extension
	GitHubRunner = "github-hosted"
	hostRegex    = `^[a-zA-Z0-9-]+\.[a-zA-Z0-9-]+.*$`
)

type Extensions struct {
	RunnerEnvironment        string
	SANRegex                 string
	SAN                      string
	BuildSourceRepoURI       string
	SignerWorkflow           string
	SourceRepositoryOwnerURI string
	SourceRepositoryURI      string
}

type Policy struct {
	Extensions    Extensions
	PredicateType string
	Artifact      artifact.DigestedArtifact
	OIDCIssuer    string
}

func newPolicy(opts *Options, a artifact.DigestedArtifact) (Policy, error) {
	p := Policy{
		Artifact: a,
	}

	if opts.SignerRepo != "" {
		signedRepoRegex := expandToGitHubURL(opts.Tenant, opts.SignerRepo)
		p.Extensions.SANRegex = signedRepoRegex
	} else if opts.SignerWorkflow != "" {
		validatedWorkflowRegex, err := validateSignerWorkflow(opts)
		if err != nil {
			return Policy{}, err
		}

		p.Extensions.SANRegex = validatedWorkflowRegex
	} else {
		p.Extensions.SANRegex = opts.SANRegex
		p.Extensions.SAN = opts.SAN
	}

	if opts.DenySelfHostedRunner {
		p.Extensions.RunnerEnvironment = GitHubRunner
	} else {
		p.Extensions.RunnerEnvironment = "*"
	}

	if opts.Repo != "" {
		if opts.Tenant != "" {
			p.Extensions.BuildSourceRepoURI = fmt.Sprintf("https://%s.ghe.com/%s", opts.Tenant, opts.Repo)
		}
		p.Extensions.BuildSourceRepoURI = fmt.Sprintf("https://github.com/%s", opts.Repo)
	}

	if opts.Tenant != "" {
		p.Extensions.SourceRepositoryOwnerURI = fmt.Sprintf("https://%s.ghe.com/%s", opts.Tenant, opts.Owner)
	} else {
		p.Extensions.SourceRepositoryOwnerURI = fmt.Sprintf("https://github.com/%s", opts.Owner)
	}

	// if issuer is anything other than the default, use the user-provided value;
	// otherwise, select the appropriate default based on the tenant
	if opts.Tenant != "" {
		p.OIDCIssuer = fmt.Sprintf(verification.GitHubTenantOIDCIssuer, opts.Tenant)
	} else {
		p.OIDCIssuer = opts.OIDCIssuer
	}

	return p, nil
}

func (p *Policy) Verify(a []*api.Attestation) (bool, string) {
	filtered := verification.FilterAttestations(p.PredicateType, a)
	if len(filtered) == 0 {
		return false, fmt.Sprintf("✗ No attestations found with predicate type: %s\n", p.PredicateType)
	}

	return true, ""
}

func expandToGitHubURL(tenant, ownerOrRepo string) string {
	if tenant == "" {
		return fmt.Sprintf("(?i)^https://github.com/%s/", ownerOrRepo)
	}
	return fmt.Sprintf("(?i)^https://%s.ghe.com/%s/", tenant, ownerOrRepo)
}

func (p *Policy) buildCertificateIdentityOption() (verify.PolicyOption, error) {
	sanMatcher, err := verify.NewSANMatcher(p.Extensions.SAN, p.Extensions.SANRegex)
	if err != nil {
		return nil, err
	}

	// Accept any issuer, we will verify the issuer as part of the extension verification
	issuerMatcher, err := verify.NewIssuerMatcher("", ".*")
	if err != nil {
		return nil, err
	}

	extensions := certificate.Extensions{
		RunnerEnvironment: p.Extensions.RunnerEnvironment,
	}

	certId, err := verify.NewCertificateIdentity(sanMatcher, issuerMatcher, extensions)
	if err != nil {
		return nil, err
	}

	return verify.WithCertificateIdentity(certId), nil
}

func (p *Policy) VerifyPredicateType(a []*api.Attestation) ([]*api.Attestation, error) {
	filteredAttestations := verification.FilterAttestations(p.PredicateType, a)

	if len(filteredAttestations) == 0 {
		return nil, fmt.Errorf("✗ No attestations found with predicate type: %s\n", p.PredicateType)
	}

	return filteredAttestations, nil
}

func (p *Policy) SigstorePolicy() (verify.PolicyBuilder, error) {
	artifactDigestPolicyOption, err := verification.BuildDigestPolicyOption(p.Artifact)
	if err != nil {
		return verify.PolicyBuilder{}, err
	}

	certIdOption, err := p.buildCertificateIdentityOption()
	if err != nil {
		return verify.PolicyBuilder{}, err
	}

	policy := verify.NewPolicy(artifactDigestPolicyOption, certIdOption)
	return policy, nil
}

func validateSignerWorkflow(opts *Options) (string, error) {
	// we expect a provided workflow argument be in the format [HOST/]/<OWNER>/<REPO>/path/to/workflow.yml
	// if the provided workflow does not contain a host, set the host
	match, err := regexp.MatchString(hostRegex, opts.SignerWorkflow)
	if err != nil {
		return "", err
	}

	if match {
		return fmt.Sprintf("^https://%s", opts.SignerWorkflow), nil
	}

	if opts.Hostname == "" {
		return "", errors.New("unknown host")
	}

	return fmt.Sprintf("^https://%s/%s", opts.Hostname, opts.SignerWorkflow), nil
}

func (p *Policy) VerifyCertExtensions(results []*verification.AttestationProcessingResult) error {
	if len(results) == 0 {
		return errors.New("no attestations proccessing results")
	}

	var atLeastOneVerified bool
	for _, attestation := range results {
		if err := p.verifyCertExtensions(attestation); err != nil {
			return err
		}
		atLeastOneVerified = true
	}

	if atLeastOneVerified {
		return nil
	} else {
		return verification.ErrNoAttestationsVerified
	}
}

func (p *Policy) verifyCertExtensions(attestation *verification.AttestationProcessingResult) error {
	if p.Extensions.SourceRepositoryOwnerURI != "" {
		sourceRepositoryOwnerURI := attestation.VerificationResult.Signature.Certificate.Extensions.SourceRepositoryOwnerURI
		if !strings.EqualFold(p.Extensions.SourceRepositoryOwnerURI, sourceRepositoryOwnerURI) {
			return fmt.Errorf("expected SourceRepositoryOwnerURI to be %s, got %s", p.Extensions.SourceRepositoryOwnerURI, sourceRepositoryOwnerURI)
		}
	}

	// if repo is set, check the SourceRepositoryURI field
	if p.Extensions.SourceRepositoryURI != "" {
		sourceRepositoryURI := attestation.VerificationResult.Signature.Certificate.Extensions.SourceRepositoryURI
		if !strings.EqualFold(p.Extensions.SourceRepositoryURI, sourceRepositoryURI) {
			return fmt.Errorf("expected SourceRepositoryURI to be %s, got %s", p.Extensions.SourceRepositoryURI, sourceRepositoryURI)
		}
	}

	if p.OIDCIssuer != "" {
		certIssuer := attestation.VerificationResult.Signature.Certificate.Extensions.Issuer
		if !strings.EqualFold(p.OIDCIssuer, certIssuer) {
			if strings.Index(certIssuer, p.OIDCIssuer+"/") == 0 {
				return fmt.Errorf("expected Issuer to be %s, got %s -- if you have a custom OIDC issuer policy for your enterprise, use the --cert-oidc-issuer flag with your expected issuer", p.OIDCIssuer, certIssuer)
			}
			return fmt.Errorf("expected Issuer to be %s, got %s", p.OIDCIssuer, certIssuer)
		}
	}

	return nil
}
