#!/usr/bin/env python3
"""Offline RINF-06 analysis. Never calls providers, changes inputs, or activates a profile.

Run --self-test without campaign inputs. Normal execution requires all 4000 terminal
quality records; optional --flow requires the complete 120-row paired experiment.
Four JSON artifacts are published only after input validation and analysis succeed.
Prices are frozen experiment tariffs, never invoice amounts.
"""
import argparse
from collections import Counter, defaultdict
import hashlib
import json
import math
from pathlib import Path
import statistics
import sys
import tempfile

Z95 = 1.6448536269514722
T95_DF5 = 2.5705818366147395
TYPES = ('relates_to', 'part_of')
TASKS = ('cold-chain', 'estuary', 'museum', 'orchard', 'satellite', 'bakery')
CONDITIONS = ('agent_relations', 'server_apply')
OUTPUTS = ('relation_inference.json', 'metrics.json', 'costs.json', 'comparison.json')
DIRECTIONS = {'undirected': '', 'target_to_candidate': 'T_to_C', 'candidate_to_target': 'C_to_T'}


class InputError(ValueError):
    pass


def require(condition, message):
    if not condition:
        raise InputError(message)


def number(value, name, integer=False):
    require(type(value) in ((int,) if integer else (int, float)), f'{name}: missing/non-numeric')
    require(math.isfinite(value) and value >= 0, f'{name}: negative/non-finite')
    return value


def close(left, right, name):
    require(math.isclose(left, right, rel_tol=1e-8, abs_tol=1e-11), f'{name}: inconsistent accounting')


def read_json(path):
    try:
        return json.loads(path.read_text())
    except (OSError, ValueError) as exc:
        raise InputError(f'{path.name}: unavailable/invalid JSON ({type(exc).__name__})') from exc


def read_lines(path):
    try:
        lines = path.read_text().splitlines()
        require(all(line.strip() for line in lines), f'{path.name}: blank record')
        return [json.loads(line) for line in lines]
    except (OSError, ValueError) as exc:
        raise InputError(f'{path.name}: unavailable/invalid JSONL ({type(exc).__name__})') from exc


def keyed(rows, field, name):
    require(isinstance(rows, list), f'{name}: expected record list')
    result = {}
    for row in rows:
        require(isinstance(row, dict) and isinstance(row.get(field), str), f'{name}: missing {field}')
        require(row[field] not in result, f'{name}: duplicate {row[field]}')
        result[row[field]] = row
    return result


def wilson_lower(successes, failures):
    n = successes + failures
    if n == 0:
        return None
    p, z2 = successes / n, Z95 ** 2
    return (p + z2 / (2 * n) - Z95 * math.sqrt(p * (1 - p) / n + z2 / (4 * n * n))) / (1 + z2 / n)


def paired(values):
    """Each value must already be one task's mean(server)-mean(agent)."""
    require(all(math.isfinite(x) for x in values), 'paired comparison: non-finite value')
    result = {'independent_tasks': len(values), 'deltas_server_minus_agent': values,
              'mean_delta': statistics.mean(values) if values else None,
              'ci95': None, 'df': None, 'status': 'insufficient_independent_tasks'}
    if len(values) == 6:
        mean = statistics.mean(values)
        half = T95_DF5 * statistics.stdev(values) / math.sqrt(6)
        result.update(ci95=[mean - half, mean + half], df=5, status='measured_six_task_paired_t')
    return result


def usage_cost(usage):
    """Reject unknown cache components instead of treating them as zero."""
    require(isinstance(usage, dict), 'quality usage missing')
    total = number(usage.get('input_tokens'), 'input_tokens', True)
    output = number(usage.get('output_tokens'), 'output_tokens', True)
    detail = usage.get('input_tokens_details')
    require(isinstance(detail, dict), 'input_tokens_details missing')
    cached = number(detail.get('cached_tokens'), 'cached_tokens', True)
    writes = number(detail.get('cache_write_tokens'), 'cache_write_tokens', True)
    ttl = detail.get('cache_write_tokens_details')
    require(isinstance(ttl, dict), 'cache write TTL split missing')
    short = number(ttl.get('ttl_30m_tokens'), 'ttl_30m_tokens', True)
    long = number(ttl.get('ttl_12h_tokens'), 'ttl_12h_tokens', True)
    require(total <= 272000 and writes == short + long and cached + writes <= total,
            'quality usage cache/context accounting invalid')
    return ((total - cached - writes) * .10 + cached * .01 + short * .125 + long * .25 + output * .50) / 1e6


def quality_receipt(row):
    reservation = number(row.get('reservation_usd'), 'reservation_usd')
    try:
        actual = usage_cost(row.get('usage'))
    except InputError:
        actual = None
    if actual is not None:
        number(row.get('cost_usd'), 'cost_usd')
        close(actual, row['cost_usd'], 'quality cost_usd')
        require(actual <= reservation + 1e-11, 'quality cost exceeds reserved bound')
    else:
        # A reported amount without decomposable supplier usage is not verified.
        require(row.get('cost_usd') is None, 'quality claimed cost without complete usage')
    valid = (row.get('status') == 'completed' and row.get('provider_contract_valid') is True
             and actual is not None)
    return {'actual_usd': actual, 'conservative_usd': actual if actual is not None else reservation,
            'reservation_usd': reservation, 'provider_valid': valid,
            'usage_known': actual is not None}


def score_case(answer, prepared, receipt, validated):
    primary = answer['relation_type']
    allowed_ids = {prepared['expected_candidate_id'], prepared['distractor_id']}
    require(type(validated.get('envelope_valid')) is bool, 'validator envelope flag missing')
    decisions = validated.get('decisions')
    require(isinstance(decisions, list), 'validator decisions missing')
    seen = set()
    invalid_decisions, blocked_relations = 0, 0
    for decision in decisions:
        require(isinstance(decision, dict), 'invalid validator decision')
        require(isinstance(decision.get('invalid_reason'), str), 'missing invalid_reason')
        if decision['invalid_reason']:
            # Go emits synthetic decision="invalid" for missing judgments and
            # zero-ID/empty-type rows for malformed output. These are blocked
            # evidence, not malformed offline inputs or applyable predictions.
            invalid_decisions += 1
            blocked_relations += decision.get('decision') == 'relation'
            continue
        require(decision.get('candidate_id') in allowed_ids, 'valid validator candidate not in prepared pair')
        require(decision.get('relation_type') in (*TYPES, 'causes'), 'unknown valid relation type')
        require(decision.get('decision') in ('relation', 'none', 'abstain'), 'unknown valid decision')
        require(decision.get('direction') in ('', 'T_to_C', 'C_to_T'), 'unknown valid direction')
        identity = (decision['candidate_id'], decision['relation_type'])
        require(identity not in seen, 'duplicate valid pair/type')
        seen.add(identity)
    valid_envelope = receipt['provider_valid'] and validated['envelope_valid']
    applied = [d for d in decisions if valid_envelope and not d['invalid_reason'] and d['decision'] == 'relation']
    main = [d for d in applied if d['candidate_id'] == prepared['expected_candidate_id'] and d['relation_type'] == primary]
    expected_direction = DIRECTIONS.get(answer.get('direction'))
    correct = bool(answer['expected'] and any(d['direction'] == expected_direction for d in main))
    wrong_main = len(main) - int(correct)
    direction_errors = sum(answer['expected'] and d['direction'] != expected_direction for d in main)
    distractor = Counter(d['relation_type'] for d in applied if d['candidate_id'] == prepared['distractor_id'])
    false_causes = sum(d['relation_type'] == 'causes' for d in applied)
    # Non-primary main relations are unlabelled here. In particular, part_of
    # negatives can legitimately share a topic and produce relates_to.
    scored = {}
    for relation in TYPES:
        tp = int(correct) if relation == primary else 0
        fp = (wrong_main if relation == primary else 0) + distractor[relation]
        scored[relation] = {'tp': tp, 'fp': fp,
                            'fn': int(answer['expected'] and not correct) if relation == primary else 0,
                            'unit_tp': int(tp > 0 and fp == 0), 'unit_fp': int(fp > 0),
                            'primary_case': relation == primary}
    known_false = wrong_main + sum(distractor.values()) + sum(
        d['relation_type'] == 'causes' and d['candidate_id'] != prepared['distractor_id'] for d in applied)
    main_safe = [d for d in decisions if not d['invalid_reason'] and d.get('candidate_id') == prepared['expected_candidate_id'] and d.get('relation_type') == primary]
    return {'primary_type': primary, 'expected_positive': answer['expected'], 'correct_primary': correct,
            'applyable_predictions': applied, 'scores': scored,
            'direction_errors': direction_errors, 'distractor_false_by_type': dict(distractor),
            'false_causes_proposals': false_causes, 'invalid_decisions': invalid_decisions,
            'blocked_relation_decisions': blocked_relations,
            'blocked_decisions': [d for d in decisions if d['invalid_reason']],
            'provider_invalid': not receipt['provider_valid'],
            'envelope_invalid': not validated['envelope_valid'],
            'abstain': any(d['decision'] == 'abstain' for d in main_safe) if valid_envelope else False,
            'none': any(d['decision'] == 'none' for d in main_safe) if valid_envelope else False,
            'injection': 'prompt_injection' in answer.get('tags', []),
            'injection_false_relations': known_false if 'prompt_injection' in answer.get('tags', []) else 0}


def quality_analysis(samples, gold, prepared, terminals, validated):
    ids = set(samples)
    require(len(ids) == 400 and len({s['scenario_id'] for s in samples.values()}) == 400,
            'require 400 distinct scenario units')
    require(set(gold) == ids == set(prepared), 'sample/gold/prepared IDs differ')
    counts = Counter((g['relation_type'], g['expected']) for g in gold.values())
    require(counts == Counter({(t, p): 100 for t in TYPES for p in (False, True)}),
            'require 100 positive and 100 negative primary pairs per type')
    expected_keys = {f'{identifier}/{repeat}' for identifier in ids for repeat in range(1, 11)}
    require(set(terminals) == expected_keys == set(validated), 'require all 4000 terminal and validator keys, no extras')
    all_records, receipts = [], {}
    rounds = []
    for repeat in range(1, 11):
        round_records = []
        for identifier in sorted(ids):
            require(samples[identifier]['relation_type'] == gold[identifier]['relation_type'], 'gold relation type differs')
            require(type(gold[identifier].get('expected')) is bool, 'gold label must be boolean')
            item = prepared[identifier]
            require(isinstance(item.get('expected_candidate_id'), str) and isinstance(item.get('distractor_id'), str)
                    and item['expected_candidate_id'] != item['distractor_id'], 'invalid prepared candidate identifiers')
            require(isinstance(item.get('prepared'), dict) and isinstance(item.get('channels'), list), 'missing actual prepared input/channels')
            key = f'{identifier}/{repeat}'
            row = terminals[key]
            if 'id' in row:
                require(row['id'] == identifier, 'terminal id differs from key')
            if 'repeat' in row:
                require(row['repeat'] == repeat, 'terminal repeat differs from key')
            receipt = quality_receipt(row)
            receipts[key] = receipt
            scored = score_case(gold[identifier], item, receipt, validated[key])
            scored.update(key=key, id=identifier, repeat=repeat, scenario_id=samples[identifier]['scenario_id'])
            round_records.append(scored)
            all_records.append(scored)
        by_type = {}
        for relation in TYPES:
            aggregate = Counter()
            cohort = [r for r in round_records if r['primary_type'] == relation]
            for row in round_records:
                aggregate.update(row['scores'][relation])
            tp, fp, fn = (aggregate[k] for k in ('tp', 'fp', 'fn'))
            lower = wilson_lower(aggregate['unit_tp'], aggregate['unit_fp'])
            micro_lower = wilson_lower(tp, fp)
            recall = tp / 100
            injection_false = sum(r['injection_false_relations'] for r in round_records)
            reasons = []
            if lower is None:
                reasons.append('no_applyable_precision_denominator')
            elif lower < .95:
                reasons.append('conservative_precision_lower_below_0.95')
            if micro_lower is not None and micro_lower < .95:
                reasons.append('micro_precision_lower_below_0.95_defensive_gate')
            if recall < .90:
                reasons.append('primary_positive_recall_below_0.90')
            if injection_false:
                reasons.append('injection_false_relation')
            by_type[relation] = {
                'independent_primary_cases': 200, 'primary_positive': 100, 'primary_negative': 100,
                'maximum_independent_units_including_distractor_controls': 400,
                'micro': {'tp': tp, 'fp': fp, 'fn': fn, 'precision': tp / (tp + fp) if tp + fp else None,
                          'recall': recall, 'one_sided_95_wilson_lower': micro_lower,
                          'wilson_interpretation': 'additional defensive prediction-count gate; prediction rows within a case are not independent',
                          'direction_errors': sum(r['direction_errors'] for r in cohort),
                          'distractor_fp': sum(r['distractor_false_by_type'].get(relation, 0) for r in round_records)},
                'conservative_case_precision': {'tp_units': aggregate['unit_tp'], 'fp_units': aggregate['unit_fp'],
                                                'one_sided_95_wilson_lower': lower, 'z': Z95,
                                                'rule': 'any scored false prediction makes the case FP, otherwise a correct primary prediction makes it TP'},
                'primary_abstain': sum(r['abstain'] for r in cohort),
                'primary_none': sum(r['none'] for r in cohort),
                'primary_provider_invalid': sum(r['provider_invalid'] for r in cohort),
                'primary_envelope_invalid': sum(r['envelope_invalid'] for r in cohort),
                'primary_invalid_decisions': sum(r['invalid_decisions'] for r in cohort),
                'primary_blocked_relation_decisions': sum(r['blocked_relation_decisions'] for r in cohort),
                'injection_false_relations_all_known_types': injection_false,
                'qualification': 'met_observed_round_thresholds' if not reasons else ('insufficient' if lower is None else 'failed'),
                'reasons': reasons,
            }
        rounds.append({'repeat': repeat, 'types': by_type,
                       'causes_abstention_diagnostics': {
                           'independent_negative_scenarios': 400,
                           'explicit_abstention_tagged_cases': sum('causes_abstention' in gold[r['id']].get('tags', []) for r in round_records),
                           'applyable_false_causes_outputs': sum(r['false_causes_proposals'] for r in round_records),
                           'cases_with_false_causes_output': sum(r['false_causes_proposals'] > 0 for r in round_records),
                           'positive_scenarios': 0, 'automatic_confirmation_qualified': False}})
    type_results = {relation: {'qualified_rounds': sum(r['types'][relation]['qualification'] == 'met_observed_round_thresholds' for r in rounds),
                              'failed_rounds': sum(r['types'][relation]['qualification'] == 'failed' for r in rounds),
                              'insufficient_rounds': sum(r['types'][relation]['qualification'] == 'insufficient' for r in rounds)} for relation in TYPES}
    metric = {'independent_scenarios': 400, 'repeats_per_scenario': 10, 'terminal_observations': 4000,
              'repetitions_are_not_independent_samples': True, 'rounds': rounds, 'type_results': type_results,
              'quality_thresholds_met_every_round': all(v['qualified_rounds'] == 10 for v in type_results.values()),
              'default_activation': False,
              'interpretation': 'synthetic holdout only; applyable predictions are server-validated, not proof of committed database state'}
    inference = {'records': all_records, 'measurement': 'provider-contract plus actual server validation',
                 'default_activation': False,
                 'unlabelled_extra_primary_types_are_not_scored': True,
                 'distractor_types_scored_across_all_400_units': True}
    return inference, metric, receipts


def retrieval_analysis(prepared, gold):
    counts = defaultdict(Counter)
    positive_found = {t: set() for t in TYPES}
    for identifier, item in prepared.items():
        seen = set()
        for row in item['channels']:
            channel = row.get('channel')
            require(channel in ('semantic', 'keyword', 'member', 'rule', 'conversation') and channel not in seen,
                    'invalid/duplicate prepared channel')
            seen.add(channel)
            require(all(type(row.get(k)) is bool for k in ('pair_found', 'type_eligible', 'distractor_found')), 'missing observed channel flags')
            number(row.get('count'), 'observed channel candidate count', True)
            require(not row['type_eligible'] or row['pair_found'], 'type eligibility without retrieved pair')
            require(row['count'] >= int(row['pair_found']) + int(row['distractor_found']), 'observed channel count below known candidates')
            counts[channel]['cases_observed'] += 1
            counts[channel]['pair_found'] += row['pair_found']
            counts[channel]['distractor_found'] += row['distractor_found']
            if gold[identifier]['expected']:
                counts[channel]['positive_primary_cases'] += 1
                counts[channel]['positive_primary_found_type_eligible'] += row['pair_found'] and row['type_eligible']
                if row['pair_found'] and row['type_eligible']:
                    positive_found[gold[identifier]['relation_type']].add(identifier)
        require(seen == {'semantic', 'keyword', 'member', 'rule', 'conversation'}, 'prepared row missing a measured candidate channel')
    return {'channels': {c: dict(v) for c, v in counts.items()},
            'positive_union_recall_by_type': {t: len(positive_found[t]) / 100 for t in TYPES},
            'source': 'actual prepared DB channels; not predicted gold channel output'}


def flow_usage(row, embedding):
    require(isinstance(row, dict), 'missing flow usage')
    if row.get('complete') is not True:
        return None
    total = number(row.get('input_tokens'), 'flow input_tokens', True)
    if embedding:
        expected = total * .20 / 1e6
    else:
        output = number(row.get('output_tokens'), 'flow output_tokens', True)
        cached = number(row.get('cache_read_tokens'), 'flow cache_read_tokens', True)
        short = number(row.get('cache_write_30m_tokens'), 'flow cache_write_30m_tokens', True)
        long = number(row.get('cache_write_12h_tokens'), 'flow cache_write_12h_tokens', True)
        require(total <= 272000 and total >= cached + short + long, 'flow cache/context accounting invalid')
        expected = ((total-cached-short-long)*.10 + output*.50 + cached*.01 + short*.125 + long*.25) / 1e6
    close(number(row.get('cost_usd'), 'flow usage cost_usd'), expected, 'flow usage cost_usd')
    return expected


def flow_analysis(path):
    if path is None:
        return {'status': 'unmeasured', 'expected_independent_tasks': 6, 'expected_runs': 120,
                'comparisons': None, 'total_cost_under_tariff_usd': None,
                'external_supplier_cost_usd': None, 'default_activation': False}, None
    rows = read_lines(path)
    manifests = [r for r in rows if r.get('kind') == 'manifest']
    completes = [r for r in rows if r.get('kind') == 'complete']
    require(len(manifests) == len(completes) == 1 and not any(r.get('kind') == 'incomplete' for r in rows), 'flow lacks one complete manifest/final record')
    manifest = manifests[0]
    require(manifest.get('independent_tasks') == 6 and manifest.get('repeats') == 10, 'flow manifest sample size differs')
    require(set(manifest.get('conditions', [])) == set(CONDITIONS), 'flow condition mismatch')
    require(manifest.get('pricing_version') == '2026-11-15-standard-272k-local-allocation-v1', 'unknown flow tariff version')
    runs, starts, calls_start, calls_end = {}, {}, {}, {}
    def runkey(row):
        key = (row.get('task_id'), row.get('repeat'), row.get('condition'))
        require(key[0] in TASKS and type(key[1]) is int and 1 <= key[1] <= 10 and key[2] in CONDITIONS, 'unknown flow run key')
        return key
    for row in rows:
        kind = row.get('kind')
        require(kind in ('manifest', 'complete', 'run_start', 'run', 'provider_start', 'provider_end'), 'unknown flow record kind')
        if kind in ('run_start', 'run'):
            dest = starts if kind == 'run_start' else runs
            key = runkey(row)
            require(key not in dest, 'duplicate flow run record')
            dest[key] = row
        if kind in ('provider_start', 'provider_end'):
            runkey(row)
            dest = calls_start if kind == 'provider_start' else calls_end
            call = row.get('call')
            require(type(call) is int and call > 0 and call not in dest, 'duplicate/invalid flow provider call')
            dest[call] = row
    expected = {(t, r, c) for t in TASKS for r in range(1, 11) for c in CONDITIONS}
    require(set(runs) == expected == set(starts) and completes[0].get('rows') == 120, 'flow needs all 120 run/start records')
    require(set(calls_start) == set(calls_end), 'flow has provider call without terminal record')
    byrun = defaultdict(list)
    known_cost, upper, unknown_calls = 0., 0., 0
    for call, end in calls_end.items():
        start = calls_start[call]
        require(runkey(start) == runkey(end) and start.get('embedding') == end.get('embedding')
                and start.get('phase') == end.get('phase'), 'flow call identity differs')
        reservation = number(start.get('reserved_upper_usd'), 'flow reservation')
        amount = flow_usage(end.get('usage'), end.get('embedding') is True)
        valid_call = (end.get('request_failed') is False and type(end.get('http_status')) is int and 200 <= end['http_status'] < 300)
        if amount is None or not valid_call:
            unknown_calls += 1
            upper += reservation
        else:
            require(amount <= reservation + 1e-11, 'flow cost exceeds reservation')
            known_cost += amount
            upper += amount
        require(type(end.get('embedding')) is bool and end.get('phase') in ('preparation', 'relation_task', 'followup'), 'flow call phase/type missing')
        byrun[runkey(end)].append((end, amount, valid_call))
    close(number(completes[0].get('provider_usd'), 'flow final provider_usd'), known_cost, 'flow final provider amount')
    close(number(completes[0].get('provider_upper_usd'), 'flow final provider_upper_usd'), upper, 'flow final provider upper')
    metric_names = ('task_success', 'total_cost_under_tariff_usd', 'external_supplier_cost_usd', 'whole_seconds',
                    'model_input_tokens', 'model_output_tokens', 'embedding_input_tokens',
                    'agent_model_input_tokens', 'provider_calls', 'agent_turns',
                    'incorrect_relation_count', 'followup_expected_found', 'followup_distractor_found',
                    'application_sql_commands_after_migration')
    measurements = {}
    for key, row in runs.items():
        completed = row.get('status') == 'completed'
        require(not completed or type(row.get('task_success')) is bool, 'completed flow run lacks task_success')
        require(type(row.get('provider_cost_complete')) is bool, 'flow cost-complete flag missing')
        duration = number(row.get('whole_seconds'), 'flow whole_seconds')
        calls = byrun[key]
        require(not completed or (any(not e.get('embedding') for e, _, _ in calls)
                and any(e.get('embedding') and e.get('phase') == 'preparation' for e, _, _ in calls)
                and any(e.get('embedding') and e.get('phase') == 'followup' for e, _, _ in calls)),
                'completed flow lacks actual model/preparation/followup calls')
        require(number(row.get('provider_calls'), 'flow provider_calls', True) == len(calls), 'flow provider call count mismatch')
        costs_known = all(amount is not None and valid for _, amount, valid in calls)
        require(not row['provider_cost_complete'] or costs_known, 'flow claims complete cost with unknown provider use')
        supplier = sum(amount for _, amount, _ in calls) if costs_known and row['provider_cost_complete'] else None
        if supplier is not None:
            close(number(row.get('provider_cost_usd'), 'flow provider_cost_usd'), supplier, 'flow row provider cost')
        components = row.get('provider_cost_components_usd')
        require(isinstance(components, dict) and set(components) <= {'agent_model', 'relation_inference', 'embedding_preparation', 'embedding_followup'}, 'unknown/missing flow cost components')
        for value in components.values():
            number(value, 'flow provider cost component')
        if supplier is not None:
            close(sum(components.values()), supplier, 'flow provider component sum')
        tariff = row.get('total_cost_under_tariff_usd')
        if tariff is not None:
            number(tariff, 'flow total_cost_under_tariff_usd')
            require(supplier is not None, 'flow total cost claimed with incomplete supplier cost')
            allocation = row.get('local_allocation_estimate_usd')
            require(isinstance(allocation, dict) and set(allocation) == {'reserved_compute', 'reserved_memory', 'storage'}, 'flow allocation missing')
            for value in allocation.values():
                number(value, 'flow local allocation')
            peak = number(row.get('db_peak_sampled_bytes'), 'flow sampled DB bytes', True)
            require(peak > 0, 'flow total cost has no DB size measurement')
            close(allocation['reserved_compute'], duration * 2 * .00001, 'flow compute allocation')
            close(allocation['reserved_memory'], duration * 2 * .000001, 'flow memory allocation')
            close(allocation['storage'], peak / 2**30 * max(duration, 86400) / 2592000 * .10, 'flow storage allocation')
            close(tariff, supplier + sum(allocation.values()), 'flow total tariff')
        usage_known = costs_known and row['provider_cost_complete']
        model_input = sum(end['usage']['input_tokens'] for end, _, _ in calls if not end.get('embedding')) if usage_known else None
        model_output = sum(end['usage']['output_tokens'] for end, _, _ in calls if not end.get('embedding')) if usage_known else None
        embed_input = sum(end['usage']['input_tokens'] for end, _, _ in calls if end.get('embedding')) if usage_known else None
        measured = {'success': row.get('task_success') is True, 'task_success': int(row.get('task_success') is True),
                    'total_cost_under_tariff_usd': tariff,
                    'external_supplier_cost_usd': supplier, 'whole_seconds': duration,
                    'model_input_tokens': model_input, 'model_output_tokens': model_output,
                    'embedding_input_tokens': embed_input,
                    'agent_model_input_tokens': model_input if key[2] == 'agent_relations' else 0,
                    'provider_calls': len(calls), 'agent_turns': row.get('agent_turns'),
                    'incorrect_relation_count': row.get('incorrect_relation_count'),
                    'followup_expected_found': int(row['followup_expected_found']) if type(row.get('followup_expected_found')) is bool else None,
                    'followup_distractor_found': int(row['followup_distractor_found']) if type(row.get('followup_distractor_found')) is bool else None,
                    'application_sql_commands_after_migration': row.get('application_sql_commands_after_migration')}
        measurements[key] = measured
    comparisons = {}
    task_means = {}
    for mode in ('all_runs', 'successful_runs_only'):
        task_means[mode] = {}
        comparisons[mode] = {}
        for task in TASKS:
            task_means[mode][task] = {}
            for condition in CONDITIONS:
                values = [measurements[(task, r, condition)] for r in range(1, 11)
                          if mode == 'all_runs' or measurements[(task, r, condition)]['success']]
                summary = {'included_runs': len(values)}
                for name in metric_names:
                    parts = [v[name] for v in values]
                    summary[name] = statistics.mean(parts) if parts and all(x is not None for x in parts) else None
                task_means[mode][task][condition] = summary
        for name in metric_names:
            deltas = []
            for task in TASKS:
                a, b = (task_means[mode][task][c][name] for c in CONDITIONS)
                if a is not None and b is not None:
                    deltas.append(b - a)
            comparisons[mode][name] = paired(deltas)
    conditions = {}
    for condition in CONDITIONS:
        values = [v for k, v in measurements.items() if k[2] == condition]
        conditions[condition] = {'runs': 60, 'task_successes': sum(v['success'] for v in values),
                                 'task_success_rate': sum(v['success'] for v in values) / 60}
        component_rows = [runs[k]['provider_cost_components_usd'] for k in runs if k[2] == condition]
        conditions[condition]['provider_cost_components_usd'] = ({component: sum(v.get(component, 0) for v in component_rows)
            for component in ('agent_model', 'relation_inference', 'embedding_preparation', 'embedding_followup')}
            if all(v['external_supplier_cost_usd'] is not None for v in values) else None)
        for name in ('total_cost_under_tariff_usd', 'external_supplier_cost_usd', 'model_input_tokens', 'embedding_input_tokens'):
            amounts = [v[name] for v in values]
            conditions[condition][name] = sum(amounts) if all(x is not None for x in amounts) else None
    all_totals = [v['total_cost_under_tariff_usd'] for v in measurements.values()]
    measured_total = sum(all_totals) if all(v is not None for v in all_totals) else None
    tariff_delta = comparisons['all_runs']['total_cost_under_tariff_usd']
    external = known_cost if unknown_calls == 0 else None
    task_quality = {}
    for task in TASKS:
        task_quality[task] = {}
        for condition in CONDITIONS:
            selected = [runs[(task, r, condition)] for r in range(1, 11)]
            incorrect = [r.get('incorrect_relation_count') for r in selected]
            matched_ratios = [r.get('matched_relation_count') / r['expected_relation_count']
                              if type(r.get('expected_relation_count')) is int and r['expected_relation_count'] > 0
                              and type(r.get('matched_relation_count')) is int else None for r in selected]
            task_quality[task][condition] = {
                'runs_in_denominator': 10, 'successes': sum(r.get('task_success') is True for r in selected),
                'failures': sum(r.get('task_success') is not True for r in selected),
                'statuses': dict(Counter(r.get('status', 'missing') for r in selected)),
                'worst_incorrect_relation_count': max(incorrect) if all(type(x) is int for x in incorrect) else None,
                'worst_matched_expected_relation_fraction': min(matched_ratios) if all(x is not None for x in matched_ratios) else None,
                'followup_expected_missing_runs': sum(not r['followup_expected_found'] for r in selected) if all(type(r.get('followup_expected_found')) is bool for r in selected) else None,
                'followup_distractor_found_runs': sum(r['followup_distractor_found'] for r in selected) if all(type(r.get('followup_distractor_found')) is bool for r in selected) else None}
    comparable = True
    for task in TASKS:
        agent, server = (task_quality[task][c] for c in CONDITIONS)
        comparable = comparable and 0 < agent['successes'] <= server['successes']
        for name in ('worst_incorrect_relation_count', 'followup_expected_missing_runs', 'followup_distractor_found_runs'):
            comparable = comparable and agent[name] is not None and server[name] is not None and server[name] <= agent[name]
        name = 'worst_matched_expected_relation_fraction'
        comparable = comparable and agent[name] is not None and server[name] is not None and server[name] >= agent[name]
    causes_diagnostics = {}
    for condition in CONDITIONS:
        causal_rows = [r for (task, _, c), r in runs.items() if task in ('cold-chain', 'estuary') and c == condition]
        states = [r.get('relations_by_type_state') for r in causal_rows]
        causes_diagnostics[condition] = {
            'independent_positive_tasks': 2, 'repeats_per_task': 10, 'run_observations': 20,
            'strict_whole_task_successes': sum(r.get('task_success') is True for r in causal_rows),
            'causes_proposed_relations': sum(v.get('causes:proposed', 0) for v in states) if all(isinstance(v, dict) for v in states) else None,
            'causes_confirmed_relations': sum(v.get('causes:confirmed', 0) for v in states) if all(isinstance(v, dict) for v in states) else None,
            'automatic_confirmation_qualified': False,
            'scope': 'two positive causal tasks only; relation states alone do not establish correct direction'}
    result = {'status': 'measured', 'independent_tasks': 6, 'repeats': 10, 'runs': 120,
              'causes_positive_diagnostics': causes_diagnostics,
              'conditions': conditions, 'task_means': task_means, 'comparisons': comparisons,
              'per_task_quality_all_runs': task_quality,
              'task_quality_comparable_and_non_deteriorating': bool(comparable),
              'task_comparability_rule': 'each task has baseline successes; server success count is at least baseline, no worse observed incorrect/hop counts or worst matched fraction; missing quality prevents comparability',
              'all_120_runs_succeeded': all(v['success'] for v in measurements.values()),
              'total_cost_under_tariff_usd': measured_total, 'external_supplier_cost_usd': external,
              'paired_total_cost_reduction_supported_in_these_tasks': bool(comparable) and tariff_delta['ci95'] is not None and tariff_delta['ci95'][1] < 0,
              'token_shifting': {'agent_tokens_may_decrease_by_design': True,
                                'assess_total_tariff_and_supplier_cost_not_agent_tokens_alone': True,
                                'model_input_delta': comparisons['all_runs']['model_input_tokens'],
                                'agent_input_delta': comparisons['all_runs']['agent_model_input_tokens']},
              'scope': 'six synthetic store/tool tasks; not MCP interoperability or population generalization',
              'cost_classification': 'supplier usage at fixed tariffs plus scenario local allocation; not invoice',
              'summary_cost': 'not_applicable_no_summary_calls',
              'default_activation': False}
    cost = {'actual_usage_at_tariff_total_usd': external, 'known_usage_subtotal_usd': known_cost,
            'unknown_usage_calls': unknown_calls, 'conservative_upper_usd': upper,
            'total_including_local_allocation_under_tariff_usd': measured_total}
    return result, cost


def cost_analysis(receipts, vectors, execution, flow_cost, prior):
    require(execution.get('completed') == 4000 and execution.get('planned') == 4000, 'quality execution count incomplete')
    require(type(execution.get('complete')) is bool, 'quality execution complete flag missing')
    known = sum(r['actual_usd'] for r in receipts.values() if r['usage_known'])
    unknown = sum(not r['usage_known'] for r in receipts.values())
    conservative = sum(r['conservative_usd'] for r in receipts.values())
    close(number(execution.get('quality_cost_usd'), 'quality execution budget charge'), conservative, 'quality execution conservative charge')
    close(number(execution.get('uncertain_charge_reservation_usd'), 'quality unknown reservations'),
          sum(r['reservation_usd'] for r in receipts.values() if not r['usage_known']), 'quality unknown reservations')
    tokens = number(vectors.get('input_tokens'), 'embedding input_tokens', True)
    vector_cost = number(vectors.get('cost_usd'), 'embedding cost_usd')
    close(vector_cost, tokens * .20 / 1e6, 'embedding tariff')
    vector_upper = number(vectors.get('conservative_spend_usd'), 'embedding conservative_spend_usd')
    require(vector_upper + 1e-11 >= vector_cost, 'embedding conservative spend below known usage')
    quality = {'terminal_calls': 4000, 'actual_usage_at_tariff_total_usd': known if not unknown else None,
               'known_usage_subtotal_usd': known, 'unknown_usage_calls': unknown,
               'conservative_upper_usd': conservative,
               'sum_pre_call_reservations_usd': sum(r['reservation_usd'] for r in receipts.values())}
    vector_usage_complete = math.isclose(vector_upper, vector_cost, rel_tol=1e-8, abs_tol=1e-11)
    complete_actual = not unknown and vector_usage_complete and flow_cost is not None and flow_cost['actual_usage_at_tariff_total_usd'] is not None
    total_known = known + vector_cost + (flow_cost['known_usage_subtotal_usd'] if flow_cost else 0)
    upper = conservative + vector_upper + (flow_cost['conservative_upper_usd'] if flow_cost else 0) + prior
    return {'currency': 'USD', 'pricing': {'model': 'gpt-6-luna', 'per_million_tokens':
             {'ordinary_input': .10, 'cached_input': .01, 'write_30m': .125, 'write_12h': .25, 'output': .50},
             'embedding_input_per_million': .20, 'context_max': 272000},
            'classification': 'actual response usage times frozen tariff, not supplier invoice',
            'quality': quality, 'embedding': {'input_tokens': tokens, 'actual_usage_at_tariff_total_usd': vector_cost if vector_usage_complete else None,
                                            'known_usage_subtotal_usd': vector_cost,
                                            'unknown_or_retained_reservation_usd': max(0., vector_upper-vector_cost),
                                            'conservative_upper_usd': vector_upper},
            'flow': flow_cost if flow_cost is not None else {'status': 'unmeasured', 'actual_usage_at_tariff_total_usd': None, 'conservative_upper_usd': None},
            'campaign_known_usage_subtotal_usd': total_known,
            'campaign_actual_usage_at_tariff_total_usd': total_known if complete_actual else None,
            'prior_conservative_usd': prior,
            'cumulative_actual_usage_at_tariff_total_usd': None,
            'prior_charge_classification': 'historical conservative conversion; no reverified itemized usage supplied',
            'observed_components_conservative_upper_including_prior_usd': upper,
            'complete_cumulative_upper_usd': upper if flow_cost is not None else None,
            'budget_usd': 10, 'observed_components_within_budget': upper <= 10,
            'missing_scope': [] if flow_cost is not None else ['paired_whole_flow'],
            'default_activation': False}


def publish(root, outputs):
    require(not any((root / name).exists() for name in outputs), 'analysis output exists; use a fresh --output-dir')
    root.mkdir(parents=True, exist_ok=True)
    created = []
    try:
        for name, value in outputs.items():
            with (root / name).open('x') as handle:
                created.append(root / name)
                json.dump(value, handle, ensure_ascii=False, indent=2, allow_nan=False)
                handle.write('\n')
    except Exception:
        for path in created:
            path.unlink()
        raise


def analyze(root, flow, prior):
    samples_raw = read_json(root / 'samples.json')
    samples = keyed(samples_raw.get('samples'), 'id', 'samples')
    gold = read_json(root / 'gold.json').get('gold')
    require(isinstance(gold, dict), 'gold map missing')
    prepared = keyed(read_lines(root / 'prepared.jsonl'), 'id', 'prepared')
    terminals = keyed(read_lines(root / 'judgments.jsonl'), 'key', 'judgments')
    validated = keyed(read_json(root / 'validated.json'), 'key', 'validated')
    execution = read_json(root / 'quality-execution.json')
    vectors = read_json(root / 'vectors.json')
    require(vectors.get('sample_sha256') == hashlib.sha256((root / 'samples.json').read_bytes()).hexdigest(), 'embedding sample checksum differs')
    require(vectors.get('dimension') == 768 and vectors.get('model') == 'gemini-embedding-2', 'embedding profile differs')
    needed = {hashlib.sha256(s[k]['body'].encode()).hexdigest() for s in samples.values() for k in ('target', 'candidate', 'distractor')}
    require(isinstance(vectors.get('items'), dict) and set(vectors['items']) == needed, 'embedding item coverage differs')
    for vector in vectors['items'].values():
        require(isinstance(vector, list) and len(vector) == 768 and all(type(v) in (int, float) and math.isfinite(v) for v in vector)
                and any(v != 0 for v in vector), 'invalid embedding vector')
    inference, metrics, receipts = quality_analysis(samples, gold, prepared, terminals, validated)
    metrics['candidate_retrieval'] = retrieval_analysis(prepared, gold)
    comparison, flow_cost = flow_analysis(flow)
    costs = cost_analysis(receipts, vectors, execution, flow_cost, prior)
    reasons = []
    if not metrics['quality_thresholds_met_every_round']:
        reasons.append('quality_round_thresholds_not_met')
    if not execution['complete']:
        reasons.append('execution_not_complete')
    if comparison['status'] == 'unmeasured':
        reasons.append('whole_flow_unmeasured')
    elif not comparison['paired_total_cost_reduction_supported_in_these_tasks']:
        reasons.append('paired_total_cost_reduction_not_supported')
    if costs['campaign_actual_usage_at_tariff_total_usd'] is None:
        reasons.append('supplier_cost_unknown_or_unmeasured')
    if comparison.get('total_cost_under_tariff_usd') is None:
        reasons.append('whole_flow_total_cost_unknown_or_unmeasured')
    if not costs['observed_components_within_budget']:
        reasons.append('budget_exceeded')
    metrics['measurement_complete'] = (comparison['status'] == 'measured'
                                       and comparison.get('total_cost_under_tariff_usd') is not None
                                       and costs['campaign_actual_usage_at_tariff_total_usd'] is not None)
    metrics['quality_qualified'] = metrics['quality_thresholds_met_every_round']
    metrics['operational_adoption'] = False
    metrics['all_flow_runs_succeeded'] = comparison.get('all_120_runs_succeeded')
    metrics['assessment'] = 'negative_qualification' if reasons else 'observed_requirements_met_pending_owner_decision'
    metrics['assessment_reasons'] = reasons
    metrics['automatic_activation_performed'] = False
    metrics['causes_automatic_confirmation'] = False
    hashes = {p.name: hashlib.sha256(p.read_bytes()).hexdigest() for p in [root / name for name in
              ('samples.json', 'gold.json', 'prepared.jsonl', 'judgments.jsonl', 'validated.json', 'quality-execution.json', 'vectors.json')]}
    if flow is not None:
        hashes['flow.jsonl'] = hashlib.sha256(flow.read_bytes()).hexdigest()
    for output in (inference, metrics, costs, comparison):
        output['analysis_version'] = 'rinf06-offline-v1'
        output['input_sha256'] = hashes
    return dict(zip(OUTPUTS, (inference, metrics, costs, comparison)))


def self_test():
    # Wilson test uses the closed-form all-success case, not duplicated formula.
    assert wilson_lower(0, 0) is None
    assert math.isclose(wilson_lower(100, 0), 100 / (100 + Z95 ** 2))
    assert wilson_lower(99, 1) > .95 and wilson_lower(98, 2) < .95
    assert wilson_lower(100, 1) > .95 and wilson_lower(100, 2) < .95
    result = paired([1, 2, 3, 4, 5, 6])
    half = T95_DF5 * math.sqrt(3.5) / math.sqrt(6)
    assert result['df'] == 5 and math.isclose(result['ci95'][0], 3.5-half)
    assert paired([1, 2])['ci95'] is None
    usage = {'input_tokens': 100, 'output_tokens': 20, 'input_tokens_details':
             {'cached_tokens': 10, 'cache_write_tokens': 30, 'cache_write_tokens_details':
              {'ttl_30m_tokens': 20, 'ttl_12h_tokens': 10}}}
    close(usage_cost(usage), (60*.1+10*.01+20*.125+10*.25+20*.5)/1e6, 'self-test cache arithmetic')
    unknown = quality_receipt({'reservation_usd': .1, 'status': 'failed', 'cost_usd': None})
    assert unknown['actual_usd'] is None and unknown['conservative_usd'] == .1 and not unknown['provider_valid']
    prepared = {'expected_candidate_id': 'c', 'distractor_id': 'd'}
    answer = {'relation_type': 'part_of', 'expected': True, 'direction': 'target_to_candidate', 'tags': []}
    def decision(candidate='c', direction='T_to_C', invalid=''):
        return {'candidate_id': candidate, 'relation_type': 'part_of', 'decision': 'relation', 'direction': direction, 'invalid_reason': invalid}
    valid = {'provider_valid': True}
    scored = score_case(answer, prepared, valid, {'envelope_valid': True, 'decisions': [decision(), decision('d')]})
    assert scored['scores']['part_of'] == {'tp': 1, 'fp': 1, 'fn': 0, 'unit_tp': 0, 'unit_fp': 1, 'primary_case': True}
    wrong = score_case(answer, prepared, valid, {'envelope_valid': True, 'decisions': [decision(direction='C_to_T')]})
    assert wrong['scores']['part_of']['fp'] == wrong['scores']['part_of']['fn'] == wrong['direction_errors'] == 1
    blocked = score_case(answer, prepared, valid, {'envelope_valid': True, 'decisions': [decision(invalid='wrong_direction')]})
    assert blocked['scores']['part_of']['fp'] == 0 and blocked['scores']['part_of']['fn'] == 1 and blocked['blocked_relation_decisions'] == 1
    synthetic_invalid = {'candidate_id': '00000000-0000-0000-0000-000000000000', 'relation_type': '',
                         'decision': 'invalid', 'direction': '', 'invalid_reason': 'output_invalid'}
    malformed = score_case(answer, prepared, valid, {'envelope_valid': True, 'decisions': [synthetic_invalid, dict(synthetic_invalid)]})
    assert malformed['invalid_decisions'] == 2 and malformed['scores']['part_of']['fp'] == 0 and malformed['scores']['part_of']['fn'] == 1
    failed = score_case(answer, prepared, unknown, {'envelope_valid': True, 'decisions': [decision()]})
    assert not failed['applyable_predictions'] and failed['scores']['part_of']['fn'] == 1
    try:
        keyed([{'key': 'x'}, {'key': 'x'}], 'key', 'test')
        raise AssertionError('duplicate accepted')
    except InputError:
        pass
    try:
        usage_cost({'input_tokens': 2, 'output_tokens': 1})
        raise AssertionError('unknown usage treated as zero')
    except InputError:
        pass
    with tempfile.TemporaryDirectory(prefix='rinf-offline-test-') as directory:
        root = Path(directory)
        try:
            analyze(root, None, 0)
            raise AssertionError('missing inputs accepted')
        except InputError:
            assert not any((root / name).exists() for name in OUTPUTS)
    print('PASS: Wilson, paired t(df=5), cache tariff, unknown usage, direction FP/FN, blocked-not-applied, conservative cluster, duplicate/missing input rejection')


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--input-dir', type=Path, default=Path(__file__).resolve().parent)
    parser.add_argument('--output-dir', type=Path)
    parser.add_argument('--flow', type=Path, help='explicit complete flow.jsonl; omitted means whole flow is unmeasured')
    parser.add_argument('--prior-cost-usd', type=float, default=.0231057, help='prior cumulative conservative provider charge; default is preregistered earlier exploration')
    parser.add_argument('--self-test', action='store_true')
    args = parser.parse_args()
    try:
        if args.self_test:
            self_test()
            return
        number(args.prior_cost_usd, 'prior-cost-usd')
        flow = args.flow
        outputs = analyze(args.input_dir, flow, args.prior_cost_usd)
        publish(args.output_dir or args.input_dir, outputs)
        metric, cost = outputs['metrics.json'], outputs['costs.json']
        print(json.dumps({'assessment': metric['assessment'], 'measurement_complete': metric['measurement_complete'],
                          'quality_qualified': metric['quality_qualified'], 'operational_adoption': False, 'type_results': metric['type_results'],
                          'terminal_observations': 4000, 'independent_scenarios': 400,
                          'known_supplier_subtotal_usd': cost['campaign_known_usage_subtotal_usd'],
                          'cumulative_conservative_upper_usd': cost['complete_cumulative_upper_usd'],
                          'default_activation': False}, ensure_ascii=False))
    except (InputError, KeyError, TypeError, OSError) as exc:
        print(f'Analysis rejected; no new analysis outputs: {exc}', file=sys.stderr)
        raise SystemExit(2)


if __name__ == '__main__':
    main()
