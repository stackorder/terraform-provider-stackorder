package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/types"

	v1 "github.com/stackorder/stackorder/api/v1"

	"github.com/stackorder/terraform-provider-stackorder/internal/client"
	"github.com/stackorder/terraform-provider-stackorder/internal/outputs"
)

const (
	codeNotFound   = "not_found"
	codeConflict   = "conflict"
	codeSuperseded = "superseded"
	codeInvalid    = "invalid"
)

var (
	_ datasource.DataSource              = (*outputsDataSource)(nil)
	_ datasource.DataSourceWithConfigure = (*outputsDataSource)(nil)
)

type outputsDataSource struct {
	data *providerData
}

type outputsDataSourceModel struct {
	Stack              types.String  `tfsdk:"stack"`
	ID                 types.String  `tfsdk:"id"`
	Values             types.Dynamic `tfsdk:"values"`
	NonsensitiveValues types.Dynamic `tfsdk:"nonsensitive_values"`
}

type stackOutputs struct {
	id                 string
	values             types.Dynamic
	nonsensitiveValues types.Dynamic
}

// NewOutputsDataSource returns the stackorder_outputs data source, which
// reads the outputs a stack shares through the Stackorder server.
func NewOutputsDataSource() datasource.DataSource {
	return &outputsDataSource{}
}

// Metadata names the data source stackorder_outputs.
func (d *outputsDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_outputs"
}

// Schema describes the stack the data source names and the values it reads.
func (d *outputsDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Reads the root-module outputs a stack shares through the Stackorder server, so a stack can use another stack's outputs without access to its state.\n\n" +
			"Nothing is shared until the producer opts in on its default branch: its `.stackorder.yaml`, or `stackorder.yaml` for every stack of its repository, lists the stacks that may read its outputs under `outputs.readers`. " +
			"Sensitive outputs are withheld unless the producer also sets `outputs.sensitive: share`, and the data source warns about the outputs withheld.\n\n" +
			"Values read by a data source are stored in the plan file, which Stackorder uploads as a workflow artifact, and in state. " +
			"Read sensitive values with the `stackorder_outputs` ephemeral resource instead, which keeps them out of both.",
		Attributes: map[string]schema.Attribute{
			"stack": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "Key of the producer stack. `path` or `path:instance` names a stack of the provider's `repository`, such as `stacks/prod/vpc` or `stacks/prod/apps:blue`; " +
					"`owner/repo//key` names a stack of another repository of the same Stackorder installation, such as `acme/platform//stacks/prod/dns`.",
			},
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Qualified key of the stack, `owner/repo//key`.",
			},
			"values": schema.DynamicAttribute{
				Computed:  true,
				Sensitive: true,
				MarkdownDescription: "Object with one attribute per output the stack serves, named after the output, sensitive outputs included when the producer shares them. " +
					"Terraform treats the whole object as sensitive. Outputs the producer withholds are absent.",
			},
			"nonsensitive_values": schema.DynamicAttribute{
				Computed: true,
				MarkdownDescription: "The object of `values` without the outputs marked sensitive. " +
					"Terraform does not treat it as sensitive, so it can feed arguments that refuse sensitive values, such as `for_each`.",
			},
		},
	}
}

// Configure keeps the settings of the configured provider for Read.
func (d *outputsDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	data, ok := req.ProviderData.(*providerData)
	if !ok {
		resp.Diagnostics.AddError("Unexpected provider data",
			fmt.Sprintf("The stackorder_outputs data source received provider data of type %T. Report this to the provider's maintainers.", req.ProviderData))
		return
	}
	d.data = data
}

// Read requests the stack's outputs from the Stackorder server.
func (d *outputsDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var model outputsDataSourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &model)...)
	if resp.Diagnostics.HasError() {
		return
	}
	read, diags := readStackOutputs(ctx, d.data, model.Stack.ValueString())
	resp.Diagnostics.Append(diags...)
	if diags.HasError() {
		return
	}
	model.ID = types.StringValue(read.id)
	model.Values = read.values
	model.NonsensitiveValues = read.nonsensitiveValues
	resp.Diagnostics.Append(resp.State.Set(ctx, &model)...)
}

func readStackOutputs(ctx context.Context, data *providerData, stack string) (stackOutputs, diag.Diagnostics) {
	var diags diag.Diagnostics
	if data == nil {
		diags.AddError("Unconfigured stackorder provider", "The stackorder provider was not configured before the read. Report this to the provider's maintainers.")
		return stackOutputs{}, diags
	}
	repo, key, problem := producerStack(stack, data.Repository)
	if problem != nil {
		diags.Append(problem)
		return stackOutputs{}, diags
	}
	id := v1.QualifiedStackKey(repo, key)
	out, err := data.Client.StackOutputs(ctx, repo, key, data.RunID, data.Reader)
	if err != nil {
		diags.Append(readFailure(err, data, repo, key))
		return stackOutputs{}, diags
	}
	values, err := outputs.Object(out.Outputs, nil)
	var nonsensitive types.Dynamic
	if err == nil {
		nonsensitive, err = outputs.Object(out.Outputs, func(_ string, o v1.StackOutput) bool { return !o.Sensitive })
	}
	if err != nil {
		diags.AddError("Malformed stack outputs", fmt.Sprintf(
			"The Stackorder server at %s served outputs of %s that the provider cannot convert: %s. Report this to the server's operator.",
			data.ServerHost, id, err))
		return stackOutputs{}, diags
	}
	if len(out.Withheld) > 0 {
		diags.Append(withheldWarning(id, out.Withheld))
	}
	return stackOutputs{id: id, values: values, nonsensitiveValues: nonsensitive}, diags
}

func producerStack(stack, repository string) (repo, key string, problem diag.Diagnostic) {
	if !strings.Contains(stack, "//") {
		switch {
		case stack == "":
			return "", "", stackError("Invalid stack", "The stack is empty. Name the producer stack as path, path:instance or owner/repo//key.")
		case !validKey(stack):
			return "", "", stackError("Invalid stack", fmt.Sprintf(
				"%q is not the key of a stack. Name the producer stack as path, path:instance or owner/repo//key.", stack))
		case repository == "":
			return "", "", stackError("Missing Stackorder repository", fmt.Sprintf(
				"The stack %q has no owner/repo// prefix, so it is a stack of the repository being planned, and no setting names that repository. Set the provider's repository, %s or %s to owner/repo, or name the stack as owner/repo//%s.",
				stack, envRepository, envGitHubRepository, stack))
		case !validRepository(repository):
			return "", "", stackError("Invalid Stackorder repository", fmt.Sprintf(
				"The stack %q has no owner/repo// prefix, so it belongs to the repository named by the provider's repository, else %s, else %s, and the first of them set holds %q, which is not owner/repo. Set it to owner/repo, such as acme/infra, or name the stack as owner/repo//%s.",
				stack, envRepository, envGitHubRepository, repository, stack))
		}
		return repository, stack, nil
	}
	repo, key = v1.SplitQualifiedStackKey(stack)
	if !validRepository(repo) || !validKey(key) {
		return "", "", stackError("Invalid stack", fmt.Sprintf(
			"%q is not the key of a stack of another repository. Write owner/repo//key, such as acme/platform//stacks/prod/dns.", stack))
	}
	return repo, key, nil
}

func validRepository(repo string) bool {
	owner, name, ok := strings.Cut(repo, "/")
	return ok && validSegment(owner) && validSegment(name)
}

func validSegment(s string) bool {
	return s != "" && s != "." && s != ".." && url.PathEscape(s) == s
}

func validKey(key string) bool {
	return key != "" && key != "." && key != ".."
}

func readFailure(err error, data *providerData, repo, key string) diag.Diagnostic {
	var apiErr *client.APIError
	if errors.As(err, &apiErr) {
		return refusal(apiErr, data, repo, key)
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		if host := hostOf(urlErr.URL); host != data.ServerHost {
			return diag.NewErrorDiagnostic("GitHub Actions ID token unavailable", fmt.Sprintf(
				"The provider could not request a GitHub Actions ID token from %s: %s. Retry later.", host, urlErr.Err))
		}
		return diag.NewErrorDiagnostic("Stackorder server unreachable", fmt.Sprintf(
			"The provider could not reach the Stackorder server at %s: %s. Check the server URL in server_url or %s, and that the server is reachable from where Terraform runs, then retry later.",
			data.ServerHost, urlErr.Err, envServerURL))
	}
	return diag.NewErrorDiagnostic("Cannot read stack outputs", sentence(err.Error()))
}

func refusal(e *client.APIError, data *providerData, repo, key string) diag.Diagnostic {
	producer := v1.QualifiedStackKey(repo, key)
	host := data.ServerHost
	stackRefusal := func(summary, detail string) diag.Diagnostic {
		return stackError(summary, detail+"\n\n"+answer(e))
	}
	refused := func(summary, detail string) diag.Diagnostic {
		return diag.NewErrorDiagnostic(summary, detail+"\n\n"+answer(e))
	}
	switch e.Code {
	case v1.ErrCodeOutputsNotShared:
		reader, entry := readerEntry(data)
		return stackRefusal("Stack outputs not shared", fmt.Sprintf(
			"The stack %s does not share its outputs with %s: either outputs.readers on its default branch does not list it, or that outputs configuration does not resolve, as the server's answer below says. A producer shares its outputs only with the stacks it lists under outputs.readers on its default branch, in the stack's .stackorder.yaml or in stackorder.yaml for every stack of its repository:\n\n"+
				"  outputs:\n    readers:\n      - %s\n\n"+
				"The server reads the list from the default branch only, so a change takes effect once it is merged.",
			producer, reader, entry))
	case v1.ErrCodeOutputsDisabled:
		return refused("Shared outputs disabled", fmt.Sprintf(
			"The Stackorder server at %s does not serve stack outputs: they stay off until the server's operator sets STACKORDER_STATE_BUCKETS to the state buckets, or bucket/prefix pairs, the server may read. Ask the operator to enable them.",
			host))
	case v1.ErrCodeStateLocationUnknown:
		return stackRefusal("State location unknown", fmt.Sprintf(
			"The Stackorder server does not know yet which state object holds the state of %s. It learns the location only from the stack's apply and drift jobs, when they run a Stackorder CLI that reports the state object. Run one apply or drift job of the producer with a current Stackorder CLI, such as the next scheduled drift run, then read again.",
			producer))
	case codeNotFound:
		detail := fmt.Sprintf(
			"The Stackorder server knows no stack %s that this reader may read. Check that the key is exact, path or path:instance as on the producer's default branch, and that the stack exists on the default branch of %s.",
			producer, repo)
		if !strings.EqualFold(data.Repository, repo) {
			detail += " A stack of another repository must belong to the same Stackorder installation and list the reader under outputs.readers: the server answers not found rather than disclose a stack of another repository that does not share with the reader."
		}
		return stackRefusal("Stack not found", detail+" A server release without shared outputs answers not found as well.")
	case v1.ErrCodeStateNotFound:
		return stackRefusal("State object not found", fmt.Sprintf(
			"The state object the Stackorder server recorded for %s, from its last apply or drift job, does not exist. If the producer moved its state to another bucket, key or workspace, its next apply or drift job records the new location; otherwise ask the server's operator to check the object.",
			producer))
	case v1.ErrCodeStateAccessDenied:
		return stackRefusal("State object access denied", fmt.Sprintf(
			"S3 or KMS denied the Stackorder server read access to the state object of %s. The server's operator must grant the server's role s3:GetObject on the object, s3:ListBucket on its bucket and prefix, without which S3 reports a missing object as denied too, and kms:Decrypt on the state's KMS key through S3.",
			producer))
	case v1.ErrCodeStateBucketNotAllowed:
		return stackRefusal("State bucket not allowed", fmt.Sprintf(
			"The state object of %s lies outside the buckets and prefixes the Stackorder server may read. The server's operator must add its bucket, or bucket/prefix, to STACKORDER_STATE_BUCKETS and grant the server's role read access to it.",
			producer))
	case v1.ErrCodeStateEncrypted:
		return stackRefusal("State encrypted", fmt.Sprintf(
			"The state of %s is encrypted by OpenTofu client-side state encryption, which the Stackorder server never decrypts, so the stack cannot share its outputs through Stackorder.",
			producer))
	case v1.ErrCodeStateUnsupported:
		return stackRefusal("State format unsupported", fmt.Sprintf(
			"The state of %s is in a format version the Stackorder server does not read. The server reads version 4 state, which Terraform 0.12 and later and every OpenTofu release write; an apply of the producer with such a release rewrites the state in it.",
			producer))
	case v1.ErrCodeStateTooLarge:
		return stackRefusal("State too large", fmt.Sprintf(
			"The state object of %s, or its outputs, exceed the size limits of the Stackorder server for the state objects it scans and the outputs it serves. Reduce the outputs of the producer, or split the stack.",
			producer))
	case v1.ErrCodeStateUnreadable:
		return stackRefusal("State unreadable", fmt.Sprintf(
			"The Stackorder server could not decode the state object of %s as Terraform or OpenTofu state. Check that the object at the stack's state location is an intact state file; the next apply of the producer rewrites it.",
			producer))
	case codeConflict:
		return refused("Stackorder run finished", fmt.Sprintf(
			"The Stackorder server at %s serves stack outputs to a job only while the job's run is in progress, and the run in %s has finished. Start a new run, for example by pushing or commenting stackorder plan, and read the outputs in its jobs.",
			host, envRunID))
	case codeSuperseded:
		return refused("Stackorder run superseded", fmt.Sprintf(
			"The run in %s was superseded by a run for a newer commit, and the Stackorder server at %s serves no stack outputs to a superseded run. The jobs of the newer run read them.",
			envRunID, host))
	case codeInvalid:
		return refused("Invalid read of stack outputs", fmt.Sprintf(
			"The Stackorder server at %s rejected the read of %s as malformed. The provider names the run from %s and the reading stack from %s: check that they hold a run id and a stack key as the Stackorder CLI sets them, or leave both unset outside a Stackorder job.",
			host, producer, envRunID, envStack))
	}
	switch {
	case e.Status == http.StatusUnauthorized && data.IDTokens:
		return refused("Stackorder credentials rejected", fmt.Sprintf(
			"The Stackorder server at %s rejected the job's GitHub Actions ID token. Check that the provider's oidc_audience, else %s, else the server URL, names an audience the server accepts.",
			host, client.EnvOIDCAudience))
	case e.Status == http.StatusUnauthorized:
		return refused("Stackorder credentials rejected", fmt.Sprintf(
			"The Stackorder server at %s rejected the API key from api_key or %s. Check that it is a current automation key of this server that nobody revoked.",
			host, client.EnvAPIKey))
	case e.Status == http.StatusForbidden && data.IDTokens:
		return refused("Stackorder server refused the read", fmt.Sprintf(
			"The Stackorder server at %s accepted the job's GitHub Actions ID token but refused this read. It serves outputs to an ID token only for a reading stack of the job's run whose job has not reported its result yet: the run in %s and the stack in %s, which the Stackorder CLI sets for Terraform in its plan, apply and drift jobs.",
			host, envRunID, envStack))
	case e.Status == http.StatusForbidden:
		return refused("Stackorder server refused the read", fmt.Sprintf(
			"The Stackorder server at %s refused the API key from api_key or %s for this read.", host, client.EnvAPIKey))
	case redirected(e.Status):
		return refused("Stackorder server redirected the read", fmt.Sprintf(
			"The Stackorder server at %s answered the read of %s with a redirect, which the provider never follows so that its credentials reach only the configured server. Check that server_url or %s names the Stackorder server itself rather than a sign-in page or proxy in front of it.",
			host, producer, envServerURL))
	case e.Status == http.StatusTooManyRequests || e.Status >= http.StatusInternalServerError:
		return refused("Stackorder server unavailable", fmt.Sprintf(
			"The Stackorder server at %s could not serve the outputs of %s. Retry later; if the error persists, the server's operator finds its cause in the server's logs.",
			host, producer))
	}
	return refused("Unexpected answer from the Stackorder server", fmt.Sprintf(
		"The Stackorder server at %s did not serve the outputs of %s.", host, producer))
}

func readerEntry(data *providerData) (reader, entry string) {
	if data.Reader == "" {
		return "the reading stack", "<key of the reading stack>"
	}
	return "the stack " + data.Reader, data.Reader
}

func answer(e *client.APIError) string {
	s := "The server answered " + strconv.Itoa(e.Status)
	if e.Code == "" {
		if text := http.StatusText(e.Status); text != "" {
			s += " " + text
		}
		if host := redirectHost(e); host != "" {
			s += ", redirecting to " + host
		}
		return s + "."
	}
	s += " " + e.Code
	if msg := strings.TrimRight(e.Message, ". "); msg != "" {
		s += ": " + msg
	}
	return s + "."
}

func redirectHost(e *client.APIError) string {
	target, ok := strings.CutPrefix(e.Message, "redirected to ")
	if !redirected(e.Status) || !ok {
		return ""
	}
	target, _, _ = strings.Cut(target, ", which is never followed")
	return hostOf(target)
}

func redirected(status int) bool {
	return status >= http.StatusMultipleChoices && status < http.StatusBadRequest
}

func sentence(s string) string {
	s = strings.TrimRight(s, ". ")
	first, size := utf8.DecodeRuneInString(s)
	if size == 0 {
		return s
	}
	return string(unicode.ToUpper(first)) + s[size:] + "."
}

func withheldWarning(producer string, withheld []string) diag.Diagnostic {
	names := make([]string, len(withheld))
	for i, name := range slices.Sorted(slices.Values(withheld)) {
		names[i] = strconv.Quote(name)
	}
	return diag.NewWarningDiagnostic("Sensitive outputs withheld", fmt.Sprintf(
		"The stack %s withholds its sensitive outputs %s, so they are absent from values. The producer shares them by setting outputs.sensitive: share beside outputs.readers on its default branch. Shared sensitive outputs are stored in the reader's plan file and state; read them with the stackorder_outputs ephemeral resource to keep them out of both.",
		producer, strings.Join(names, ", ")))
}

func stackError(summary, detail string) diag.Diagnostic {
	return diag.NewAttributeErrorDiagnostic(path.Root("stack"), summary, detail)
}
