// SPDX-License-Identifier: Elastic-2.0
// Package doctor explains already observed configuration state. It has no
// transport, credentials, database access, clock, or mutation capability.
package doctor

import (
	"fmt"
	"slices"
	"time"

	"agentplatform.local/platform/internal/domain"
)

const Version = "1"

type Finding struct {
	Code        string `json:"code"`
	Level       string `json:"level"`
	Subject     string `json:"subject,omitempty"`
	Message     string `json:"message"`
	Remediation string `json:"remediation,omitempty"`
}

type Report struct {
	ReportVersion  string    `json:"report_version"`
	APIContract    string    `json:"api_contract"`
	Assessment     string    `json:"assessment"`
	OrganizationID string    `json:"organization_id,omitempty"`
	StartedAt      time.Time `json:"started_at"`
	FinishedAt     time.Time `json:"finished_at"`
	Revision       string    `json:"configuration_revision,omitempty"`
	Complete       bool      `json:"inspection_complete"`
	Status         string    `json:"status"`
	ReadOnly       bool      `json:"read_only"`
	ExternalProbes int       `json:"external_probes"`
	Coverage       string    `json:"coverage"`
	Findings       []Finding `json:"findings"`
}

type Snapshot struct {
	Catalog       domain.ConfigurationCatalog
	Destinations  []domain.ConfigurationDestination
	Resources     []domain.ConfigurationResource
	Verifications []domain.ConfigurationVerification
	Plan          *domain.ConfigurationPlan
}

// Commands are templates for a separate explicit invocation. Never execute
// them, substitute peer text, or treat the catalog as a permission grant.
type Remediation struct {
	ID           string     `json:"id"`
	Actor        string     `json:"actor"`
	Message      string     `json:"message"`
	Commands     [][]string `json:"command_templates"`
	Requirements []string   `json:"requirements"`
	ExternalIO   string     `json:"external_io"`
}

type Catalog struct {
	Version      string        `json:"report_version"`
	APIContract  string        `json:"api_contract"`
	ReadOnly     bool          `json:"doctor_read_only"`
	Scope        string        `json:"scope"`
	Remediations []Remediation `json:"remediations"`
}

func Remediations() Catalog {
	return Catalog{Version, domain.ContractVersion, true, "configuration diagnostics; templates do not grant permission or run automatically", []Remediation{
		{"inspect_access", "current_user_or_organization_owner", "Проверьте адрес платформы, версию клиента и текущий доступ к выбранной организации. При отказе запросите минимально необходимое разрешение у владельца.", [][]string{{"platform", "config", "catalog", "--json"}}, []string{"PLATFORM_URL", "private credentials or PLATFORM_TOKEN and PLATFORM_ORGANIZATION_ID"}, "platform_reads_only"},
		{"install_destination", "trusted_installation_operator", "Оператор регистрирует разрешённое назначение и назначение ключа. Пустой видимый список не доказывает отсутствие старых или недоступных подключений.", [][]string{{"platform", "install-credential-purpose", "--file", "<private-purpose.json>"}, {"platform", "install-configuration-destination", "--file", "<private-destination.json>"}}, []string{"trusted host access", "approved endpoint and credential purpose", "docs/CONFIGURATION.md"}, "none"},
		{"create_plan", "authorized_user_or_delegated_agent", "Прочитайте текущие версии и создайте точный план. Для старого подключения без configuration binding существующую версию уточняет оператор; не выводите версию 0 из пустого списка.", [][]string{{"platform", "config", "catalog", "--json"}, {"platform", "config", "resources", "list", "--json"}, {"platform", "config", "plans", "create", "--file", "<private-plan.json>", "--key", "<new-explicit-key>", "--json"}}, []string{"current configuration revision", "exact destination and existing resource versions", "current scoped authority", "contracts/CONFIGURATION.md"}, "none"},
		{"validate_plan", "plan_actor", "Выполните статическую проверку точной версии и digest плана; она не проверяет связь с поставщиком и не обращается к модели.", [][]string{{"platform", "config", "plans", "validate", "--id", "<plan-id>", "--file", "<private-validation.json>", "--key", "<explicit-key>", "--json"}}, []string{"fresh exact plan version and digest", "current authorization"}, "none"},
		{"apply_plan", "plan_actor_and_owner_if_required", "После статической проверки получите требуемое одобрение владельца и отдельно примените точный план. Сохранение настроек не подтверждает соединение.", [][]string{{"platform", "config", "plans", "approve", "--id", "<plan-id>", "--file", "<private-approval.json>", "--key", "<explicit-key>", "--json"}, {"platform", "config", "plans", "apply", "--id", "<plan-id>", "--file", "<private-apply.json>", "--key", "<explicit-key>", "--json"}}, []string{"fresh validation ID", "exact plan digest/version", "owner approval only when required", "current authority and resource versions"}, "none"},
		{"verify_connection", "authorized_plan_actor", "Отдельно запустите ограниченную проверку одной операции применённого плана с явным согласием на расходы. Модель может расходовать деньги; MCP допускает только заранее разрешённое бесплатное discovery. Существующая проверка не заменяется новой после неопределённого результата.", [][]string{{"platform", "config", "plans", "verify", "--id", "<plan-id>", "--file", "<private-verification.json>", "--key", "<explicit-key>", "--json"}}, []string{"current applied plan and active exact resource", "one operation ID", "explicit cost acknowledgment and limit", "no previous verification for this operation", "current authority"}, "explicit_bounded_model_or_mcp_probe"},
		{"inspect_verification", "authorized_reader", "Прочитайте существующую проверку. При неизвестном исходе не создавайте замену и не отправляйте действие повторно; расследуйте сохранённые факты и исходную операцию.", [][]string{{"platform", "config", "verifications", "get", "--id", "<verification-id>", "--json"}}, []string{"existing verification ID", "current read permission"}, "platform_reads_only"},
		{"reconcile_cost", "trusted_billing_operator", "Проверьте финансовое обязательство по достоверным данным поставщика. Успешное соединение не закрывает резерв и не доказывает окончательный счёт; doctor не изменяет финансовый журнал.", [][]string{}, []string{"authenticated provider evidence", "contracts/COSTS.md", "no settlement based only on model response"}, "operator_defined"},
		{"repeat_inspection", "current_reader", "Повторите только чтение состояния. При превышении границы списка используйте обычную пагинацию; неполная диагностика не подтверждает отсутствие проблемы.", [][]string{{"platform", "doctor", "--json"}}, []string{"current credentials", "stable configuration for a useful observation"}, "platform_reads_only"},
	}}
}

func New(start time.Time) Report {
	return Report{ReportVersion: Version, APIContract: domain.ContractVersion, Assessment: "configuration_observation", StartedAt: start.UTC(), Status: "incomplete", ReadOnly: true, Coverage: "current caller's visible configuration bindings; excludes legacy connections, current external availability, worker secrets and production readiness", Findings: []Finding{}}
}

func (r *Report) Add(code, level, subject, message, remediation string) {
	r.Findings = append(r.Findings, Finding{code, level, subject, message, remediation})
}

func (r *Report) Finish(complete bool, at time.Time) {
	r.FinishedAt, r.Complete = at.UTC(), complete
	r.Status = "observed"
	if !complete {
		r.Status = "incomplete"
		return
	}
	for _, f := range r.Findings {
		if f.Level == "attention" || f.Level == "unknown" {
			r.Status = "attention_required"
		}
	}
}

func Evaluate(r *Report, s Snapshot) {
	r.OrganizationID, r.Revision = s.Catalog.OrganizationID, fmt.Sprint(s.Catalog.Revision)
	r.Add("catalog_observed", "info", "", "Прочитан каталог текущей организации. Перечень действий не является разрешением на их выполнение.", "")
	if !slices.Contains(s.Catalog.AvailableCommands, "create_plan") {
		r.Add("planning_not_available", "attention", "", "Каталог не сообщает право создать план от текущей учётной записи.", "inspect_access")
	}
	for _, kind := range []string{"model", "mcp"} {
		count := 0
		for _, d := range s.Destinations {
			if d.Kind == kind && d.Active {
				count++
			}
		}
		if count == 0 {
			r.Add("no_visible_active_destination", "attention", kind, "В доступном списке нет активного разрешённого назначения этого типа. Старые и скрытые права доступа этим не проверены.", "install_destination")
		}
	}
	for _, d := range s.Destinations {
		if !d.Active {
			continue
		}
		subject := fmt.Sprintf("%s:%s:%d", d.Kind, d.ID, d.Version)
		found := false
		for _, resource := range s.Resources {
			if resource.Active && matchesDestination(resource.Scope, d) {
				found = true
			}
		}
		if !found {
			r.Add("destination_not_applied", "attention", subject, "Для этой точной версии назначения не найдено активное доступное подключение, созданное через план настройки.", "create_plan")
		}
		if d.Kind == "mcp" && !d.DiscoveryUnmeteredApproved {
			r.Add("discovery_not_approved", "attention", subject, "Оператор не подтвердил бесплатное discovery. Проверка MCP этим профилем не разрешена.", "install_destination")
		}
	}
	for _, resource := range s.Resources {
		if !resource.Active {
			continue
		}
		subject := fmt.Sprintf("%s:%s:%d", resource.Kind, resource.ID, resource.Version)
		trusted := false
		for _, d := range s.Destinations {
			if d.Active && matchesDestination(resource.Scope, d) {
				trusted = true
			}
		}
		if !trusted {
			r.Add("resource_destination_unavailable", "attention", subject, "Точное активное назначение подключения отсутствует в доступном наблюдении. Не обходите серверную проверку доверия.", "inspect_access")
		}
		var receipt *domain.ConfigurationVerification
		for i := range s.Verifications {
			v := &s.Verifications[i]
			if v.Scope == resource.Scope && v.ResourceVersion == resource.Version && (receipt == nil || v.CreatedAt.After(receipt.CreatedAt)) {
				receipt = v
			}
		}
		if receipt == nil {
			r.Add("verification_not_observed", "unknown", subject, "Не найдена доступная проверка точной версии подключения. Возможно, проверка недоступна этой учётной записи. Doctor не обращался к внешнему сервису.", "verify_connection")
		}
	}
	if s.Plan != nil {
		p := s.Plan
		switch p.Status {
		case "applied":
			r.Add("plan_applied", "info", p.ID, "План применён. Это не подтверждает доступность подключения или правильность ответа модели.", "")
			for _, op := range p.Operations {
				if !op.Active {
					continue
				}
				active, verified := false, false
				for _, resource := range s.Resources {
					active = active || resource.Active && resource.Scope == op.Scope && resource.Version == op.ExpectedResourceVersion+1
				}
				for _, v := range s.Verifications {
					verified = verified || v.PlanID == p.ID && v.OperationID == op.ID && v.Scope == op.Scope && v.ResourceVersion == op.ExpectedResourceVersion+1
				}
				if !active {
					r.Add("plan_resource_not_current", "attention", op.ID, "Точная применённая версия отсутствует среди доступных активных подключений. Исторический результат проверки не подтверждает текущую настройку.", "create_plan")
				} else if !verified {
					r.Add("plan_operation_unverified", "unknown", op.ID, "Доступная проверка этой операции не найдена. Запуск возможен только отдельной командой и после текущих серверных проверок.", "verify_connection")
				}
			}
		case "planned":
			if p.BaseRevision != s.Catalog.Revision {
				r.Add("plan_revision_stale", "attention", p.ID, "План основан на другой версии конфигурации. Прочитайте изменения перед созданием нового плана.", "create_plan")
			} else {
				r.Add("plan_not_applied", "attention", p.ID, "План ещё не применён. Нужны свежая статическая проверка, требуемое согласование и отдельное применение.", "validate_plan")
			}
		default:
			r.Add("plan_not_applicable", "attention", p.ID, "Текущее состояние плана не позволяет считать настройку выполненной. Старые внешние эффекты и расходы сохраняются.", "create_plan")
		}
	}
	for _, v := range s.Verifications {
		switch v.Status {
		case "response_received":
			r.Add("probe_response_recorded", "info", v.ID, "Сохранён ответ на прежнюю проверку соединения. Это не проверка доступности сейчас, бизнес-результата или всех возможностей MCP.", "")
		case "unknown":
			r.Add("probe_outcome_unknown", "attention", v.ID, "Исход отправленной проверки неизвестен. Не повторяйте отправку и не создавайте замену; сохраните прежние обязательства.", "inspect_verification")
		case "queued", "running", "sent":
			r.Add("probe_pending", "attention", v.ID, "Проверка ещё не имеет конечного наблюдаемого результата. Читайте существующую запись, не запускайте замену.", "inspect_verification")
		default:
			r.Add("probe_not_successful", "attention", v.ID, "Проверка не подтверждает получение ответа. Изучите сохранённую запись и причины до следующих действий.", "inspect_verification")
		}
		if v.Kind == "model" && v.CostStatus != "settled" && v.CostStatus != "released" {
			r.Add("cost_not_final", "attention", v.ID, "В наблюдении нет окончательного расчёта или освобождения резерва. Ответ модели не является счётом поставщика.", "reconcile_cost")
		}
	}
	r.Add("external_availability_not_tested", "info", "", "Этот запуск выполнял только чтение платформы. Сеть до поставщика, секреты worker, sandbox и готовность промышленного выпуска не проверены.", "")
}

func matchesDestination(s domain.ConfigurationScope, d domain.ConfigurationDestination) bool {
	return s.DestinationID == d.ID && s.DestinationVersion == d.Version && s.Kind == d.Kind && s.TargetID == d.TargetID && s.CredentialPurposeID == d.CredentialPurposeID && s.CredentialPurposeVersion == d.CredentialPurposeVersion
}
