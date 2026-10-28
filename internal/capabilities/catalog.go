// SPDX-License-Identifier: Elastic-2.0
// Package capabilities describes support. It never evaluates execution authority.
package capabilities

import (
	_ "embed"
	"encoding/json"
	"errors"
	"time"

	"agentplatform.local/platform/internal/domain"
)

//go:embed catalog.json
var encoded []byte

//go:embed bindings.json
var bindingsJSON []byte

type Definition struct {
	APIContract        string                        `json:"api_contract"`
	AuthorizationRules []string                      `json:"authorization_rules"`
	Operations         []domain.CapabilityOperation  `json:"operations"`
	Profiles           []domain.CapabilityProfile    `json:"profiles"`
	Roles              []domain.CapabilityRole       `json:"roles"`
	Onboarding         []domain.CapabilityOnboarding `json:"onboarding"`
	RemediationCommand []string                      `json:"remediation_command"`
}

// Decode a bounded local immutable asset per observation. Returned slices have
// no shared mutable backing store. No tenant connection or peer data is read.
func Observe(p domain.Principal, at time.Time) (domain.ProductCapabilities, error) {
	var d Definition
	if len(encoded) > 1<<20 || json.Unmarshal(encoded, &d) != nil || d.APIContract != domain.ContractVersion {
		return domain.ProductCapabilities{}, errors.New("capability catalogue unavailable")
	}
	return d.Observation(p, at)
}

func (d Definition) Observation(p domain.Principal, at time.Time) (domain.ProductCapabilities, error) {
	v := domain.ProductCapabilities{CatalogVersion: "1", APIContract: d.APIContract,
		Assessment: "support_and_role_observation", ReadOnly: true, ObservedAt: at.UTC(),
		Identity:           domain.CurrentIdentity{PrincipalID: p.ID, OrganizationID: p.OrganizationID, Role: p.Role, PrincipalKind: p.Kind, Permissions: domain.EffectivePermissions(p)},
		AuthorizationRules: d.AuthorizationRules, Operations: d.Operations, Profiles: d.Profiles,
		Roles: d.Roles, Onboarding: d.Onboarding, RemediationCommand: d.RemediationCommand}
	if !Valid(v) {
		return domain.ProductCapabilities{}, errors.New("capability catalogue unavailable")
	}
	return v, nil
}

// Build-time transport bindings are reviewed references to implemented clients.
// The runtime observes only the generated immutable catalogue asset.
func SurfaceBindings(method, path string) []domain.CapabilityBinding {
	var values map[string]map[string]domain.CapabilityBinding
	if json.Unmarshal(bindingsJSON, &values) != nil {
		panic("invalid capability bindings")
	}
	out := []domain.CapabilityBinding{}
	for _, surface := range []string{"python", "typescript", "workspace"} {
		binding, ok := values[method+" "+path][surface]
		if !ok {
			panic("missing capability surface binding: " + method + " " + path + " " + surface)
		}
		binding.Surface = surface
		out = append(out, binding)
	}
	return out
}

func NewDefinition(operations []domain.CapabilityOperation) Definition {
	roles := []domain.CapabilityRole{}
	for _, kind := range []string{domain.PrincipalHuman, domain.PrincipalService} {
		for _, role := range []string{"viewer", "member", "admin", "owner"} {
			if kind == domain.PrincipalService && role != "viewer" && role != "member" {
				continue
			}
			roles = append(roles, domain.CapabilityRole{PrincipalKind: kind, Role: role,
				Permissions: domain.EffectivePermissions(domain.Principal{Kind: kind, Role: role})})
		}
	}
	return Definition{APIContract: domain.ContractVersion, Operations: operations, Roles: roles,
		Profiles: profiles(), Onboarding: onboarding(), RemediationCommand: []string{"platform", "doctor", "--catalog", "--json"},
		AuthorizationRules: []string{
			"Каталог описывает поддержку и текущую роль; он не выдаёт разрешение или допуск отправки.",
			"Реальное действие заново проверяет организацию, личность, ресурс, версии, политику, согласования и бюджет.",
			"Configuration grant проверяется отдельно от permissions роли; сервисный member может иметь только точную делегированную область.",
			"MCP discovery и включение mutation tool не расширяют права учётной записи.",
			"Неизвестный отправленный эффект нельзя автоматически повторять. Счёт, эффект и приёмка результата — отдельные факты.",
			"Истечение или отзыв ключа запрещает новые admissions. Logout не отменяет долговечную работу; account/policy/grant отзывает соответствующее согласие на исполнение.",
			"Отзыв не отменяет уже начатую отправку, неизвестный эффект или финансовую обязанность; позднее наблюдение не восстанавливает права.",
			"Список поддержанных профилей не является списком установленных подключений или проверкой доступности поставщика.",
		}}
}

func profiles() []domain.CapabilityProfile {
	return []domain.CapabilityProfile{
		{ID: "text_model", Name: "Текстовые модели", Status: "bounded", Summary: "Версионированный HTTP chat-completions профиль с резервом и непроверенным результатом.", Constraints: []string{"Тариф и верхняя граница устанавливаются доверенным оператором.", "Нет автоматического повторного платного вызова; окончательный счёт поступает отдельно."}, Documentation: []string{"docs/LOCAL-MODELS.md", "docs/CONFIGURATION-PROFILES.md"}},
		{ID: "multiagent", Name: "Команды агентов", Status: "bounded", Summary: "Последовательность, параллельные задания и делегирование с отдельной проверкой.", Constraints: []string{"Закреплённые опубликованные версии, общий бюджет, срок и накопительные лимиты.", "Проверка другим агентом не доказывает истинность результата."}, Documentation: []string{"contracts/EXECUTION.md"}},
		{ID: "collaboration", Name: "Совместная работа", Status: "bounded", Summary: "Версии материалов, конфликты, явное слияние, публикация и отдельная приёмка.", Constraints: []string{"Редактирование текста не заменяет первоначальные источники выполнения.", "Нет универсального realtime/offline coediting и большого object storage."}, Documentation: []string{"contracts/MATERIALS.md"}},
		{ID: "automation", Name: "Ограниченная автономность", Status: "bounded", Summary: "Точные task grants, накопительные лимиты и остановка детерминированных повторов.", Constraints: []string{"Согласование и отправка изменяющих действий остаются отдельными.", "Нет обещания устранить все галлюцинации и циклы."}, Documentation: []string{"contracts/AUTOMATION.md"}},
		{ID: "mcp_server", Name: "MCP-интерфейс платформы", Status: "bounded", Summary: "Частный tools профиль2026-07-28: stdio и Streamable HTTP.", Constraints: []string{"Чтение по умолчанию; mutation tools требуют операторского opt-in и обычной текущей авторизации.", "SSE task events — отдельный API transport, не MCP tool."}, Documentation: []string{"docs/MCP-ADAPTER.md"}},
		{ID: "mcp_gateway", Name: "MCP-подключения", Status: "bounded", Summary: "Зарегистрированные HTTPS peers, ограниченное discovery и явные gateway действия.", Constraints: []string{"Configuration verify читает только одобренное бесплатное discovery/list, без tools/call.", "Произвольный stdio процесс не допускается этим action gateway."}, Documentation: []string{"contracts/TOOL-EXECUTIONS.md", "docs/CONFIGURATION.md"}},
		{ID: "mcp_extended", Name: "Расширенные MCP-возможности", Status: "not_implemented", Summary: "Полный внешний OAuth onboarding и расширенные peer профили пока не выпущены.", Constraints: []string{"Нет гарантированной поддержки legacy fallback, MRTR/elicitation continuation, Tasks/Apps/Skills, sampling/roots или server resources/prompts.", "Используйте только описанный частный tools профиль."}, Documentation: []string{"docs/MCP-ADAPTER.md"}},
		{ID: "configuration", Name: "Настройка агентом", Status: "bounded", Summary: "Inspect, точный план, static validation/diff, approval, apply и отдельная verify.", Constraints: []string{"Первоначальный доверенный профиль и ключи устанавливает оператор.", "CLI doctor наблюдает состояние без probes; сохранено, ответ получен и счёт закрыт — разные состояния."}, Documentation: []string{"contracts/CONFIGURATION.md", "docs/DOCTOR.md"}},
		{ID: "identity", Name: "Вход и доступ", Status: "bounded", Summary: "OIDC Code+PKCE, exact issuer policy, scoped memberships и ограниченные service keys.", Constraints: []string{"Полный корпоративный lifecycle, приглашения, SCIM/группы ещё не завершены.", "Service identity не получает human review/admin authority."}, Documentation: []string{"contracts/SECURITY.md", "contracts/MEMBERSHIPS.md"}},
		{ID: "events", Name: "Наблюдение и восстановление интерфейса", Status: "bounded", Summary: "Typed events, snapshot, ограниченный replay и SSE.", Constraints: []string{"Reconnect не разрешает повторить mutation.", "Полная retention/webhook доставка ещё не реализована."}, Documentation: []string{"contracts/EVENTS.md"}},
		{ID: "sandbox", Name: "Изоляция исполняемого кода", Status: "not_implemented", Summary: "Продуктовая sandbox, egress policy и внешний credential broker ещё не готовы.", Constraints: []string{"Настройка HTTPS peer не предоставляет изоляцию чужого процесса.", "Не подключайте произвольное исполнение кода как будто этот профиль уже есть."}, Documentation: []string{"docs/SUPPORT.md"}},
		{ID: "provider", Name: "Поставка облачному провайдеру", Status: "not_implemented", Summary: "Provider lifecycle, placement, квоты и интеграция панели ещё в разработке.", Constraints: []string{"Создание участника локальной организации не заменяет provision клиента.", "Нет принятого provider release, installer/backup/restore и нагрузочного профиля."}, Documentation: []string{"docs/SUPPORT.md"}},
		{ID: "billing", Name: "Учёт расходов", Status: "bounded", Summary: "Точные micro-USD, резерв, unknown, подтверждённые факты и quarantine.", Constraints: []string{"Usage из ответа модели не является окончательным счётом.", "Полный аутентифицированный billing ingress и сверка с реальным provider account ещё открыты."}, Documentation: []string{"contracts/COSTS.md"}},
	}
}

func onboarding() []domain.CapabilityOnboarding {
	step := func(id, text, actor, effect string, ack bool, operations ...string) domain.CapabilityStep {
		return domain.CapabilityStep{ID: id, Instruction: text, Actor: actor, Effect: effect, CostAcknowledgement: ack, OperationIDs: operations, Commands: [][]string{}}
	}
	return []domain.CapabilityOnboarding{
		{ID: "employee", Persona: "Сотрудник", Status: "partial", Summary: "Выбрать готового агента, запустить задачу с пределом и принять точный результат.", Prerequisites: []string{"Действующий доступ member или согласованная роль.", "Опубликованный агент и подготовленное разрешённое подключение."}, Steps: []domain.CapabilityStep{
			step("choose", "Прочитайте опубликованные версии агентов и выберите подходящую.", "current_user", "read_only", false, "get_agents"),
			step("run", "Создайте задачу с явным бюджетом; worker может вызвать платную модель.", "authorized_member", "queues_external_work", true, "post_tasks"),
			step("observe", "Наблюдайте результат, ожидания, резервы и неизвестные расходы. При запрете запросите минимальный доступ.", "current_reader", "read_only", false, "get_tasks_id", "get_tasks_id_costs", "get_tasks_id_candidates"),
			step("review", "Проверьте точный материал и отдельно примите, отклоните или верните с замечанием. Новая платная попытка требует отдельного действия.", "authorized_human", "platform_write", false, "post_tasks_id_review"),
		}, Documentation: []string{"contracts/EXECUTION.md", "docs/CAPABILITIES.md"}},
		{ID: "agent_developer", Persona: "Разработчик агента", Status: "partial", Summary: "Создать и опубликовать агента без изменения ядра.", Prerequisites: []string{"Право agents.write и tasks.create.", "Установленные model/tool профили и ограниченный тестовый бюджет."}, Steps: []domain.CapabilityStep{
			step("discover", "Прочитайте доступные модели и инструменты; discovery не даёт права их выполнить.", "developer", "read_only", false, "get_models", "get_tools"),
			step("publish", "Создайте draft и опубликуйте точную версию. Активная работа сохраняет прежние pins.", "developer", "platform_write", false, "post_agents", "post_agents_id_publish"),
			step("try", "Создайте ограниченную задачу и проверьте результат и расходы; неизвестный исход не отправляйте заново.", "developer", "queues_external_work", true, "post_tasks", "get_tasks_id_costs"),
		}, Documentation: []string{"contracts/README.md", "docs/DEVELOPMENT.md"}},
		{ID: "team_builder", Persona: "Создатель команды", Status: "partial", Summary: "Собрать sequence/parallel/delegate_verify из опубликованных версий.", Prerequisites: []string{"Подготовлены агенты участников.", "Определены общий бюджет, срок, глубина и политика частичного результата."}, Steps: []domain.CapabilityStep{
			step("pin", "Выберите точные опубликованные версии участников и шаблон команды.", "developer", "read_only", false, "get_agents"),
			step("publish_team", "Создайте team:coordinator и опубликуйте версию с ограниченным join.", "developer", "platform_write", false, "post_agents", "post_agents_id_publish"),
			step("observe_tree", "Запустите задачу, наблюдайте дочерние результаты и общий расход. Неполный итог не выдавайте за полный.", "authorized_member", "queues_external_work", true, "post_tasks", "get_tasks_id_children", "get_tasks_id_costs"),
		}, Documentation: []string{"contracts/EXECUTION.md", "docs/CAPABILITIES.md"}},
		{ID: "organization_admin", Persona: "Администратор организации", Status: "partial", Summary: "Настроить разрешённые подключения с отдельными проверками и согласованиями.", Prerequisites: []string{"Доверенный оператор установил purpose/destination и передал ключи worker.", "Human manager либо member с точным живым grant; owner доступен для требуемого approval."}, Steps: configurationSteps(step), Documentation: []string{"docs/CONFIGURATION-PROFILES.md", "docs/CONFIGURATION.md", "docs/DOCTOR.md"}},
		{ID: "cloud_provider", Persona: "Облачный провайдер", Status: "not_implemented", Summary: "Промышленный provision организаций и интеграция панели ещё не поддержаны.", Prerequisites: []string{"Нужны отдельные lifecycle, placement, quota, billing и recovery contracts.", "Локальный bootstrap не является готовым cloud onboarding."}, Steps: []domain.CapabilityStep{}, Documentation: []string{"docs/SUPPORT.md"}},
		{ID: "authorized_agent", Persona: "Уполномоченный агент", Status: "partial", Summary: "Разобраться в API и настроить только выданную область.", Prerequisites: []string{"Сервисный member key с ограниченным TTL/role ceiling.", "Владелец выдал grant на точные назначения, действия и накопительный предел."}, Steps: configurationSteps(step), Documentation: []string{"contracts/CAPABILITIES.md", "docs/CONFIGURATION.md", "docs/DOCTOR.md"}},
	}
}

func configurationSteps(step func(string, string, string, string, bool, ...string) domain.CapabilityStep) []domain.CapabilityStep {
	steps := []domain.CapabilityStep{
		step("inspect", "Прочитайте совместимость, свою личность, каталог и доступную область. При отказе не расширяйте её самостоятельно.", "current_reader", "read_only", false, "get_compatibility", "get_me", "get_capabilities", "get_configuration_catalog", "get_configuration_destinations", "get_configuration_resources"),
		step("plan", "Создайте точный неизменяемый план, прочитайте diff и выполните static validation без обращения к поставщику. Устаревший план пересоставляется осознанно.", "authorized_plan_actor", "platform_write", false, "post_configuration_plans", "post_configuration_plans_plan_id_validate"),
		step("approve", "Если требуется политикой, владелец одобряет точный digest и validation; service не подменяет owner.", "human_owner", "platform_write", false, "post_configuration_plans_plan_id_approve"),
		step("apply", "Примените свежий план один раз; после потери ответа прочитайте состояние и используйте исходный ключ. Сохранение не доказывает соединение.", "authorized_plan_actor", "platform_write", false, "post_configuration_plans_plan_id_apply"),
		step("verify", "Отдельно разрешите одну проверку с денежным пределом. Model response оставляет unknown bill; MCP verify делает только бесплатное одобренное discovery/list.", "authorized_plan_actor", "queues_external_work", true, "post_configuration_plans_plan_id_verify", "get_configuration_verifications_verification_id"),
		step("diagnose", "Прочитайте сохранённый receipt и doctor. Неизвестный эффект не повторяйте; счёт и приёмка результата проверяются отдельно.", "current_reader", "read_only", false, "get_configuration_verifications", "get_tasks_id_costs"),
	}
	steps[0].Commands = [][]string{{"platform", "capabilities", "--json"}, {"platform", "config", "catalog", "--json"}}
	steps[len(steps)-1].Commands = [][]string{{"platform", "doctor", "--json"}, {"platform", "doctor", "--catalog", "--json"}}
	return steps
}
