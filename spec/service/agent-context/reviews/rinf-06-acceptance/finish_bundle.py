"""Export and verify the canonical RINF-06 evidence bundle without network calls."""
import argparse
import hashlib
import json
from pathlib import Path

BASE=Path(__file__).resolve().parent
NAMES=('manifest.json','preparation.json','summary.json','cost.json','relation_inference.json')

def digest(path):return hashlib.sha256(path.read_bytes()).hexdigest()
def encoded(value):return (json.dumps(value,ensure_ascii=False,sort_keys=True,indent=2)+'\n').encode()

def require_complete(metrics,costs,comparison):
    if not metrics.get('measurement_complete') or metrics.get('terminal_observations')!=4000 or metrics.get('independent_scenarios')!=400:raise ValueError('incomplete quality measurement')
    if comparison.get('status')!='measured' or comparison.get('runs')!=120 or comparison.get('independent_tasks')!=6 or comparison.get('total_cost_under_tariff_usd') is None:raise ValueError('incomplete flow measurement')
    if costs.get('missing_scope') or costs.get('quality',{}).get('unknown_usage_calls')!=0 or costs.get('flow',{}).get('unknown_usage_calls')!=0:raise ValueError('unknown measured cost')
    upper=costs.get('complete_cumulative_upper_usd')
    if upper is None or upper<0 or upper>10:raise ValueError('cumulative budget not established')

def construct(analysis,flow):
    analysis,flow=analysis.resolve(),flow.resolve()
    load=lambda n:json.loads((analysis/n).read_text())
    metrics,costs,comparison=load('metrics.json'),load('costs.json'),load('comparison.json')
    relation=load('relation_inference.json');require_complete(metrics,costs,comparison)
    execution=json.loads((BASE/'manifest.json').read_text());review=json.loads((BASE/'review-approval.json').read_text())
    if review.get('approved') is not True or len(review.get('reviewed_ids',[]))!=400:raise ValueError('independent review missing')
    for name,sha in execution['fixed_files'].items():
        if digest(BASE/name)!=sha:raise ValueError('execution source changed: '+name)
    fm=json.loads(flow.read_text().splitlines()[0])
    if fm['profile_fingerprint']!=execution['profile_fingerprint']:raise ValueError('profile mismatch')
    sources=['manifest.json','review-approval.json','samples.json','gold.json','prepared.jsonl','vectors.json','judgments.jsonl','validated.json','quality-execution.json','FLOW_PROTOCOL.md','analyze_acceptance.py','finish_bundle.py']
    sources += [str((analysis/n).relative_to(BASE)) for n in ('metrics.json','costs.json','comparison.json','relation_inference.json')]
    sources += [str(flow.relative_to(BASE))]
    preparation={'format':'relation_acceptance_preparation_v1','independent_scenarios':400,'repeats':10,'counts':{'relates_to':{'positive':100,'negative':100},'part_of':{'positive':100,'negative':100}},'embedding_documents':len(json.loads((BASE/'vectors.json').read_text())['items']),'independent_review':{'approved':True,'review_sha256':digest(BASE/'review-approval.json')},'actual_store_candidates_and_build_input':True,'profile_fingerprint':execution['profile_fingerprint'],'source_manifest_sha256':digest(BASE/'manifest.json'),'candidate_retrieval':metrics['candidate_retrieval'],'flow_independent_tasks':6,'flow_repeats_per_condition':10,'default_activation':False}
    summary={'format':'relation_acceptance_summary_v1','measurement_complete':True,'assessment':metrics['assessment'],'assessment_reasons':metrics['assessment_reasons'],'quality_qualified':metrics['quality_qualified'],'type_results':metrics['type_results'],'flow_task_successes':{k:{'successes':v['task_successes'],'runs':v['runs']} for k,v in comparison['conditions'].items()},'cumulative_provider_upper_usd':costs['complete_cumulative_upper_usd'],'profile_fingerprint':execution['profile_fingerprint'],'operational_adoption':False,'default_activation':False,'automatic_activation_performed':False,'limitations':['synthetic controlled holdout with shared linguistic templates','six synthetic store-tool tasks; not MCP host acceptance','measured provider usage at frozen rates plus preregistered local allocation; not actual invoice']}
    values={'preparation.json':preparation,'summary.json':summary,'cost.json':costs,'relation_inference.json':relation}
    manifest={'format':'relation_acceptance_bundle_v1','source_root':'..','execution_manifest_sha256':digest(BASE/'manifest.json'),'profile_fingerprint':execution['profile_fingerprint'],'source_files':{n:digest(BASE/n) for n in sources},'outputs':{n:hashlib.sha256(encoded(v)).hexdigest() for n,v in values.items()},'analysis_dir':str(analysis.relative_to(BASE)),'flow_file':str(flow.relative_to(BASE)),'default_activation':False}
    values['manifest.json']=manifest
    return values

def main():
    p=argparse.ArgumentParser();p.add_argument('--analysis-dir',type=Path);p.add_argument('--flow',type=Path);p.add_argument('--output-dir',type=Path);p.add_argument('--verify',type=Path);p.add_argument('--self-test',action='store_true');a=p.parse_args()
    if a.self_test:
        for metrics,costs,comparison in [({}, {}, {}),({'measurement_complete':True,'terminal_observations':4000,'independent_scenarios':400},{'quality':{'unknown_usage_calls':0},'flow':{'unknown_usage_calls':0},'complete_cumulative_upper_usd':11},{'status':'measured','runs':120,'independent_tasks':6,'total_cost_under_tariff_usd':.1})]:
            try:require_complete(metrics,costs,comparison)
            except ValueError:continue
            raise AssertionError('incomplete or overbudget accepted')
        print('PASS: incomplete and overbudget bundles rejected');return
    if a.verify:
        actual=json.loads((a.verify/'manifest.json').read_text());expected=construct(BASE/actual['analysis_dir'],BASE/actual['flow_file'])
        if set(p.name for p in a.verify.iterdir())!=set(NAMES):raise ValueError('bundle file set differs')
        for name,value in expected.items():
            if (a.verify/name).read_bytes()!=encoded(value):raise ValueError('bundle mismatch: '+name)
        print('PASS: canonical bundle, source hashes, profile, counts, costs and decisions verified');return
    if not all((a.analysis_dir,a.flow,a.output_dir)):p.error('analysis-dir, flow and output-dir required')
    expected=construct(a.analysis_dir,a.flow);a.output_dir.mkdir(mode=0o700)
    for name in NAMES:
        with (a.output_dir/name).open('xb') as f:f.write(encoded(expected[name]))
    print(json.dumps(expected['summary.json'],ensure_ascii=False))

if __name__=='__main__':main()
