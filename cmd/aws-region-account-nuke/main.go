package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"

	"aws-region-account-nuke/internal/nuke"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

var accountPattern = regexp.MustCompile(`^[0-9]{12}$`)

type options struct {
	account                   string
	region                    string
	profile                   string
	serviceNames              []string
	resourceIDs               []string
	tags                      map[string]string
	deniedAccounts            []string
	execute                   bool
	confirmAccount            string
	disableDeletionProtection bool
	wait                      bool
	parallel                  int
	timeout                   time.Duration
	report                    string
}

type report struct {
	Account     string          `json:"account"`
	Region      string          `json:"region"`
	DryRun      bool            `json:"dry_run"`
	GeneratedAt time.Time       `json:"generated_at"`
	Resources   []nuke.Resource `json:"resources"`
}

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	os.Exit(run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	options, err := parseOptions(args, stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		fmt.Fprintln(stderr, "error:", err)
		return 2
	}
	if contains(options.deniedAccounts, options.account) {
		fmt.Fprintf(stderr, "error: account %s is denied and cannot be targeted\n", options.account)
		return 1
	}

	loadOptions := []func(*config.LoadOptions) error{config.WithRegion(options.region)}
	if options.profile != "" {
		loadOptions = append(loadOptions, config.WithSharedConfigProfile(options.profile))
	}
	awsConfig, err := config.LoadDefaultConfig(ctx, loadOptions...)
	if err != nil {
		fmt.Fprintln(stderr, "error: load AWS configuration:", err)
		return 1
	}
	if err := verifyAccount(ctx, awsConfig, options.account); err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}

	engine := nuke.Engine{
		Services:    selectedServices(awsConfig, options),
		Selector:    nuke.Selector{IDs: options.resourceIDs, Tags: options.tags},
		Parallel:    options.parallel,
		Wait:        options.wait,
		WaitTimeout: options.timeout,
	}
	plan, err := engine.Plan(ctx)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	printPlan(stdout, plan)
	if err := writeReport(options.report, report{
		Account: options.account, Region: options.region, DryRun: true,
		GeneratedAt: time.Now().UTC(), Resources: plan,
	}); err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	fmt.Fprintln(stdout, "Report:", options.report)

	if !options.execute || countPlanned(plan) == 0 {
		if !options.execute {
			fmt.Fprintln(stdout, "Dry run only; no resources were changed.")
		}
		return 0
	}
	if err := confirm(stdin, stdout, options.account, options.confirmAccount); err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}

	result := engine.Execute(ctx, plan)
	printResult(stdout, result)
	if err := writeReport(options.report, report{
		Account: options.account, Region: options.region, DryRun: false,
		GeneratedAt: time.Now().UTC(), Resources: result,
	}); err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	if hasFailures(result) {
		return 1
	}
	return 0
}

func parseOptions(args []string, stderr io.Writer) (options, error) {
	var result options
	var services, resourceIDs, deniedAccounts stringList
	var tags tagList

	flags := flag.NewFlagSet("aws-region-account-nuke", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.StringVar(&result.account, "account", "", "required 12-digit AWS account ID")
	flags.StringVar(&result.region, "region", "", "required AWS region")
	flags.StringVar(&result.profile, "profile", "", "AWS shared-config profile")
	flags.Var(&services, "services", "required comma-separated services: msk,elasticache,opensearch,rds")
	flags.Var(&resourceIDs, "resource-id", "resource ID, name, or ARN to select; repeatable")
	flags.Var(&tags, "tag", "tag selector in Key=Value form; repeatable")
	flags.Var(&deniedAccounts, "deny-account", "account ID that must never be targeted; repeatable")
	flags.BoolVar(&result.execute, "execute", false, "perform deletion; default is dry-run")
	flags.StringVar(&result.confirmAccount, "confirm-account", "", "account ID confirmation for unattended execution")
	flags.BoolVar(&result.disableDeletionProtection, "disable-deletion-protection", false, "disable RDS/Aurora deletion protection")
	flags.BoolVar(&result.wait, "wait", true, "wait for resources to be deleted")
	flags.IntVar(&result.parallel, "parallel", 2, "maximum parallel deletions")
	flags.DurationVar(&result.timeout, "timeout", 60*time.Minute, "deletion wait timeout per resource")
	flags.StringVar(&result.report, "report", "", "JSON report path")
	if err := flags.Parse(args); err != nil {
		return result, err
	}
	if flags.NArg() != 0 {
		return result, fmt.Errorf("unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}

	result.serviceNames = splitCommaSeparated(services)
	result.resourceIDs = nonEmpty(resourceIDs)
	result.tags = tags
	result.deniedAccounts = append(nonEmpty(deniedAccounts), splitCommaSeparated(stringList{os.Getenv("AWS_NUKE_DENY_ACCOUNTS")})...)
	if result.report == "" {
		result.report = "nuke-report-" + time.Now().UTC().Format("20060102T150405Z") + ".json"
	}
	if err := result.validate(); err != nil {
		return result, err
	}
	return result, nil
}

func (o options) validate() error {
	if !accountPattern.MatchString(o.account) {
		return errors.New("--account must be a 12-digit AWS account ID")
	}
	if o.region == "" {
		return errors.New("--region is required")
	}
	if len(o.serviceNames) == 0 {
		return errors.New("--services is required")
	}
	supported := map[string]bool{"msk": true, "elasticache": true, "opensearch": true, "rds": true}
	for _, name := range o.serviceNames {
		if !supported[name] {
			return fmt.Errorf("unsupported service %q", name)
		}
	}
	if len(o.resourceIDs) == 0 && len(o.tags) == 0 {
		return errors.New("at least one --resource-id or --tag selector is required")
	}
	if o.parallel < 1 || o.parallel > 10 {
		return errors.New("--parallel must be between 1 and 10")
	}
	if o.timeout <= 0 {
		return errors.New("--timeout must be positive")
	}
	for _, account := range o.deniedAccounts {
		if !accountPattern.MatchString(account) {
			return fmt.Errorf("invalid denied account ID %q", account)
		}
	}
	if o.confirmAccount != "" && !accountPattern.MatchString(o.confirmAccount) {
		return errors.New("--confirm-account must be a 12-digit AWS account ID")
	}
	return nil
}

func verifyAccount(ctx context.Context, awsConfig aws.Config, expected string) error {
	identity, err := sts.NewFromConfig(awsConfig).GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return fmt.Errorf("get caller identity: %w", err)
	}
	actual := aws.ToString(identity.Account)
	if actual != expected {
		return fmt.Errorf("credentials are for account %s, not requested account %s", actual, expected)
	}
	return nil
}

func selectedServices(awsConfig aws.Config, options options) []nuke.Service {
	services := make([]nuke.Service, 0, len(options.serviceNames))
	for _, name := range options.serviceNames {
		switch name {
		case "msk":
			services = append(services, nuke.NewMSKService(awsConfig))
		case "elasticache":
			services = append(services, nuke.NewElastiCacheService(awsConfig))
		case "opensearch":
			services = append(services, nuke.NewOpenSearchService(awsConfig))
		case "rds":
			services = append(services, nuke.NewRDSService(awsConfig, options.disableDeletionProtection))
		}
	}
	return services
}

func confirm(stdin io.Reader, stdout io.Writer, account, supplied string) error {
	if supplied != "" {
		if supplied != account {
			return errors.New("--confirm-account does not match --account")
		}
		return nil
	}
	fmt.Fprintf(stdout, "Type AWS account ID %s to confirm deletion: ", account)
	answer, err := bufio.NewReader(stdin).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	if strings.TrimSpace(answer) != account {
		return errors.New("account confirmation did not match")
	}
	return nil
}

func printPlan(output io.Writer, resources []nuke.Resource) {
	fmt.Fprintln(output, "STATUS\tSERVICE\tTYPE\tRESOURCE")
	for _, resource := range resources {
		fmt.Fprintf(output, "%s\t%s\t%s\t%s\n", resource.Status, resource.Service, resource.Type, resource.Name)
	}
	fmt.Fprintf(output, "\n%d planned, %d protected\n", countPlanned(resources), countStatus(resources, "protected"))
}

func printResult(output io.Writer, resources []nuke.Resource) {
	fmt.Fprintln(output, "\nDeletion result:")
	for _, resource := range resources {
		fmt.Fprintf(output, "%s\t%s\t%s", resource.Status, resource.Service, resource.Name)
		if resource.Error != "" {
			fmt.Fprint(output, "\t", resource.Error)
		}
		fmt.Fprintln(output)
	}
}

func writeReport(path string, contents report) error {
	data, err := json.MarshalIndent(contents, "", "  ")
	if err != nil {
		return fmt.Errorf("create report: %w", err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("write report %s: %w", path, err)
	}
	return nil
}

func countPlanned(resources []nuke.Resource) int { return countStatus(resources, "planned") }

func countStatus(resources []nuke.Resource, status string) int {
	count := 0
	for _, resource := range resources {
		if resource.Status == status {
			count++
		}
	}
	return count
}

func hasFailures(resources []nuke.Resource) bool { return countStatus(resources, "failed") > 0 }

func contains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func splitCommaSeparated(values []string) []string {
	var result []string
	for _, value := range values {
		result = append(result, strings.Split(value, ",")...)
	}
	return unique(nonEmpty(result))
}

func nonEmpty(values []string) []string {
	result := values[:0]
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			result = append(result, value)
		}
	}
	return result
}

func unique(values []string) []string {
	seen := make(map[string]bool, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if !seen[value] {
			seen[value] = true
			result = append(result, value)
		}
	}
	return result
}

type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }
func (s *stringList) Set(value string) error {
	*s = append(*s, value)
	return nil
}

type tagList map[string]string

func (t *tagList) String() string {
	keys := make([]string, 0, len(*t))
	for key := range *t {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	values := make([]string, 0, len(keys))
	for _, key := range keys {
		values = append(values, key+"="+(*t)[key])
	}
	return strings.Join(values, ",")
}

func (t *tagList) Set(value string) error {
	key, tagValue, found := strings.Cut(value, "=")
	if !found || strings.TrimSpace(key) == "" {
		return fmt.Errorf("tag %q must use Key=Value form", value)
	}
	if *t == nil {
		*t = make(map[string]string)
	}
	(*t)[strings.TrimSpace(key)] = tagValue
	return nil
}
