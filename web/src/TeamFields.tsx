import {t} from './i18n'
import {useEffect,useState} from 'react'
import type {Agent,ApiClient,TeamDefinition} from './api'
import {ErrorNotice} from './ui'
import {validateText} from './review'

export function validateTeamForm(team:TeamDefinition) {
  if(!['sequence','parallel','delegate_verify'].includes(team.pattern))throw new Error('Выберите поддерживаемую схему команды.')
  if(!['all','allow_partial'].includes(team.join_policy))throw new Error('Выберите правило сборки результата.')
  if(team.members.length<1||team.members.length>32)throw new Error('В команде должно быть от 1 до 32 участников.')
  if(team.pattern==='delegate_verify'&&team.members.length!==2)throw new Error('Для схемы «Исполнитель и проверяющий» нужны ровно два участника.')
  if(!Number.isInteger(team.max_depth)||team.max_depth<1||team.max_depth>16)throw new Error('Допустимая глубина команды — от 1 до 16.')
  if(!Number.isInteger(team.max_children)||team.max_children<team.members.length||team.max_children>32)throw new Error('Лимит подзадач должен покрывать состав команды и не превышать 32.')
  if(!Number.isInteger(team.max_parallel)||team.max_parallel!<1||team.max_parallel!>team.members.length)throw new Error('Параллельность должна быть от 1 до числа участников.')
  const ids=new Set<string>()
  for(const member of team.members){
    if(!member.id.trim()||ids.has(member.id)||!member.agent_id.trim()||!Number.isInteger(member.agent_version)||member.agent_version<1)throw new Error('Выберите опубликованную версию для каждого участника.')
    ids.add(member.id)
    if(member.task!==undefined){if(!member.task.trim())throw new Error('Удалите пустое задание участника или заполните его.');validateText(member.task,4096,'Задание участника')}
  }
}

export default function TeamFields({api,value,onChange,disabled}:{api:ApiClient;value:TeamDefinition;onChange:(value:TeamDefinition)=>void;disabled:boolean}) {
  const [agents,setAgents]=useState<Agent[]>([]),[cursor,setCursor]=useState<string|undefined>(),[error,setError]=useState<unknown>(null),[loading,setLoading]=useState(false)
  useEffect(()=>{const controller=new AbortController();setLoading(true);void api.agents(undefined,controller.signal).then(page=>{setAgents(page.data.filter(v=>v.published));setCursor(page.next_cursor);setError(null)}).catch(cause=>{if(!controller.signal.aborted)setError(cause)}).finally(()=>{if(!controller.signal.aborted)setLoading(false)});return()=>controller.abort()},[api])
  async function more(){setLoading(true);try{const page=await api.agents(cursor);setAgents(current=>[...new Map([...current,...page.data.filter(v=>v.published)].map(v=>[`${v.id}:${v.version}`,v])).values()]);setCursor(page.next_cursor);setError(null)}catch(cause){setError(cause)}finally{setLoading(false)}}
  function member(index:number,update:Partial<TeamDefinition['members'][number]>){onChange({...value,members:value.members.map((m,i)=>i===index?{...m,...update}:m)})}
  function remove(index:number){const members=value.members.filter((_,i)=>i!==index);onChange({...value,members,max_parallel:Math.max(1,Math.min(value.max_parallel??1,members.length))})}
  function add(){const occupied=new Set(value.members.map(v=>v.id));let number=value.members.length+1;while(occupied.has(`member-${number}`))number++;onChange({...value,members:[...value.members,{id:`member-${number}`,agent_id:'',agent_version:1}],max_children:Math.max(value.max_children,value.members.length+1)})}
  return <fieldset className="team-fields" disabled={disabled}><legend>{t("Состав команды")}</legend>
    <label className="field">{t("Схема работы")}<select aria-label={t("Схема работы")} value={value.pattern} onChange={event=>onChange({...value,pattern:event.target.value as TeamDefinition['pattern'],max_parallel:event.target.value==='parallel'?value.members.length:1})}><option value="sequence">{t("По очереди")}</option><option value="parallel">{t("Параллельно")}</option><option value="delegate_verify">{t("Исполнитель и проверяющий")}</option></select><small>{t("Участники получают закреплённые задания и материалы предыдущих этапов. Оценка агента не доказывает истинность ответа.")}</small></label>
    <ErrorNotice error={error} onRetry={()=>void more()}/>
    {!loading && !error && agents.length === 0 && <p className="notice notice-info">{cursor
      ? t('На этой странице нет опубликованных агентов. Загрузите ещё агентов ниже.')
      : t('Сначала создайте и опубликуйте агентов для подготовки и проверки. Затем вернитесь к созданию команды.')}</p>}
    {value.members.map((m,index)=><section className="team-member" key={m.id}><div className="panel-title"><strong>{value.pattern==='delegate_verify'?(index===0?t("Исполнитель"):index===1?t("Проверяющий"):t("Участник {value0}", {value0:(index+1)})):t("Участник {value0}", {value0:(index+1)})}</strong><button className="button button-small button-secondary" type="button" disabled={value.members.length===1} onClick={()=>remove(index)}>{t("Удалить участника ")}{index+1}</button></div><label className="field">{t("Агент и версия")}<select aria-label={t("Агент участника {value0}", {value0:(index+1)})} value={m.agent_id?`${m.agent_id}:${m.agent_version}`:''} onChange={event=>{const selected=agents.find(v=>`${v.id}:${v.version}`===event.target.value);if(selected)member(index,{agent_id:selected.id,agent_version:selected.version})}} required><option value="" disabled>{t("Выберите опубликованную версию")}</option>{m.agent_id&&!agents.some(v=>v.id===m.agent_id&&v.version===m.agent_version)&&<option value={`${m.agent_id}:${m.agent_version}`}>{m.agent_id} · v{m.agent_version}{t(" · закреплено")}</option>}{agents.map(agent=><option key={`${agent.id}:${agent.version}`} value={`${agent.id}:${agent.version}`}>{agent.name} · v{agent.version}{agent.team?t(" · команда"):''}{agent.model.startsWith('fixture:')?t(" · тестовый"):''}</option>)}</select></label><label className="field">{t("Задание участника ")}<span className="optional">{t("необязательно")}</span><textarea aria-label={t("Задание участника {value0}", {value0:(index+1)})} rows={2} value={m.task??''} onChange={event=>{const task=event.target.value;onChange({...value,members:value.members.map((v,i)=>{if(i!==index)return v;const {task:unused,...rest}=v;void unused;return task?{...rest,task}:rest})})}}/><small>{t("Без отдельного задания используется цель родительской задачи. До 4096 байт UTF-8.")}</small></label></section>)}
    <div className="review-actions"><button className="button button-secondary" type="button" disabled={value.members.length>=32||(value.pattern==='delegate_verify'&&value.members.length>=2)} onClick={add}>{t("Добавить участника")}</button>{cursor&&<button className="button button-secondary" type="button" disabled={loading} onClick={()=>void more()}>{t("Загрузить ещё агентов")}</button>}</div>
    <label className="field">{t("Сборка результата")}<select aria-label={t("Сборка результата")} value={value.join_policy} onChange={event=>onChange({...value,join_policy:event.target.value as TeamDefinition['join_policy']})}><option value="all">{t("Требуются материалы всех участников")}</option><option value="allow_partial">{t("Разрешить неполный сборник с пометкой")}</option></select></label>
    <div className="form-columns"><label className="field">{t("Параллельные участники")}<input aria-label={t("Параллельные участники")} type="number" min={1} max={value.members.length} value={value.max_parallel??1} disabled={value.pattern!=='parallel'} onChange={event=>onChange({...value,max_parallel:Number(event.target.value)})}/></label><label className="field">{t("Глубина вложенных команд")}<input aria-label={t("Глубина вложенных команд")} type="number" min={1} max={16} value={value.max_depth} onChange={event=>onChange({...value,max_depth:Number(event.target.value)})}/></label><label className="field">{t("Лимит подзадач за этап")}<input aria-label={t("Лимит подзадач за этап")} type="number" min={value.members.length} max={32} value={value.max_children} onChange={event=>onChange({...value,max_children:Number(event.target.value)})}/></label></div>
    <p className="aside-note">{t("Общий бюджет действует на всю задачу и её подзадачи. При создании задачи сервер закрепит версии вложенных команд и проверит конечный план: не более 128 задач и 384 попыток.")}</p>
  </fieldset>
}
