"""Read-only local fixture observations. Never dispatches or reconciles actions."""
from __future__ import annotations
import argparse
import datetime
import json
from pathlib import Path
import sqlite3
import sys

ROOT=Path(__file__).resolve().parents[2]
sys.path.insert(0,str(ROOT/'packages/python-sdk/src'))
from agent_platform import Client


def main():
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--ui-report',type=Path,default=ROOT/'web/tests/model-tools-live-report-0.7.json')
    args=parser.parse_args()
    sdk=json.loads((ROOT/'tests/sdk/live-report-0.7.json').read_text())
    contract=json.loads((ROOT/'tests/contracts/live-report-0.7.json').read_text())
    ui=json.loads(args.ui_report.read_text())
    records=[r for flow in sdk['tool_flows']+sdk['model_loop_flows'] for r in flow['results']]+contract['tool_flows']+[{**contract['native_model_flow'],'mode':'success'}]+ui['results']
    assert len({r['execution_id'] for r in records})==len(records)
    native=[r for r in records if r.get('initial_operation_id')]
    tool_db=sqlite3.connect('file:'+str(ROOT/'.local/tool-fixture-ledger.sqlite3')+'?mode=ro',uri=True)
    model_db=sqlite3.connect('file:'+str(ROOT/'.local/native-model-fixture-ledger.sqlite3')+'?mode=ro',uri=True)
    def observe():
        tools=[];models=[]
        for record in records:
            row=tool_db.execute('SELECT action_id,mode,synthetic_effect FROM occurrences WHERE execution_id=?',(record['execution_id'],)).fetchone()
            assert row is not None and row[1]==record['mode'] and row[2]==1
            count=tool_db.execute('SELECT COUNT(*) FROM requests WHERE action_id=?',(row[0],)).fetchone()[0]
            assert count==1
            tools.append({'execution_id':record['execution_id'],'action_id':row[0],'mode':row[1],'synthetic_effect':row[2],'physical_requests':count})
        for record in native:
            for phase,operation in [('initial',record['initial_operation_id']),('continued',record['continued_operation_id'])]:
                request_id='model:'+operation
                count=model_db.execute('SELECT COUNT(*) FROM requests WHERE request_id=?',(request_id,)).fetchone()[0]
                row=model_db.execute('SELECT kind,request_digest FROM responses WHERE request_id=?',(request_id,)).fetchone()
                assert count==1 and row is not None and row[0]==('proposal_'+record['mode'] if phase=='initial' else 'final')
                models.append({'request_id':request_id,'phase':phase,'kind':row[0],'request_digest':row[1],'physical_requests':count})
        return {'tools':tools,'models':models}
    before=observe()
    private=json.loads((ROOT/'.local/bootstrap.json').read_text())
    client=Client('http://127.0.0.1:8080',token=private['token'],organization_id=private['organization_id'])
    for record in records:
        task=client.get_task(record['task_id']);view=client.get_tool_execution(record['task_id'],record['execution_id'])
        assert view['execution']['status'] in ('succeeded','rejected') and view.get('receipt')
        assert view['execution']['id']==record['execution_id']
        if record.get('initial_operation_id'):
            assert task['status']=='succeeded' and task.get('accepted_decision_id') and task['attempt_count']==1
            exact=client.get_model_proposal(task['id'],record['proposal_id'])
            assert exact['execution_id']==record['execution_id']
    failed=json.loads((ROOT/'tests/sdk/live-report-0.7-initial-fixture-failure.json').read_text())
    old=client.get_tool_execution(failed['task_id'],failed['execution_id'])
    assert old['execution']['status']=='unknown'
    old_count=tool_db.execute('SELECT COUNT(*) FROM requests WHERE action_id=?',(old['execution']['action_id'],)).fetchone()[0]
    assert old_count==0
    after=observe();assert before==after
    report={'contract_version':'0.7.0','result':'pass','time':datetime.datetime.now(datetime.timezone.utc).isoformat(),
            'scope':'Read-only observation of explicit current test execution IDs in persistent local synthetic fixtures, then API reopen; no universal exactly-once guarantee',
            'fixture_tool_executions':len(records),'fixture_tool_physical_requests':sum(r['physical_requests'] for r in before['tools']),
            'fixture_tool_synthetic_effects':sum(r['synthetic_effect'] for r in before['tools']),'unknown_modes':sum(r['mode']=='unknown' for r in before['tools']),
            'native_scenarios':len(native),'native_model_requests':sum(r['physical_requests'] for r in before['models']),
            'observations_before':before,'observations_after':after,'unchanged_after_read_only_reopen':True,
            'original_unavailable_fixture_attempt':{'execution_id':failed['execution_id'],'status':'unknown','physical_requests':old_count,'resend_performed':False},
            'paid_provider_called':False,'billing_settlement':False,'peer_source_evidence':'docs/evidence/model-loop-peer-sources-0.7.json'}
    (ROOT/'tests/sdk/tool-physical-observations-0.7.json').write_text(json.dumps(report,indent=2)+'\n')
    print(json.dumps({k:report[k] for k in ['result','fixture_tool_executions','fixture_tool_physical_requests','fixture_tool_synthetic_effects','unknown_modes','native_scenarios','native_model_requests','unchanged_after_read_only_reopen']}))


if __name__=='__main__':main()
