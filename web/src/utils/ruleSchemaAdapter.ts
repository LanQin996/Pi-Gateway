import type {LocalizedHelp, RuleCapability, RuleField, RulePhase, RuleRenderer, RuleSchema} from '../api/rules'
import {help} from './ruleSchema'

export interface BackendField {
    name: string;
    type: string;
    label?: string;
    description: string;
    default?: unknown;
    required?: boolean
    enum?: string[];
    minimum?: number;
    maximum?: number;
    non_empty?: boolean;
    depends_on?: Record<string, unknown>;
    examples?: unknown[]; help?: LocalizedHelp; enum_help?: Record<string,string>
}

export interface BackendCapability {
    id: string;
    label?: string;
    description: string;
    phases?: RulePhase[];
    fields: BackendField[]; deprecated?: boolean
}

export interface BackendRuleSchema {
    schema_version: number;
    phases: RulePhase[];
    rule_fields: BackendField[];
    conditions: BackendCapability[];
    actions: BackendCapability[]
    value_expressions: BackendCapability[];
    context_fields: BackendField[];
    examples?: NonNullable<RuleSchema['examples']>;
    limits?: Record<string, number>
}

export const backendRenderers: Record<string, RuleRenderer> = {
    string: 'string',
    boolean: 'boolean',
    integer: 'number',
    pointer: 'string',
    string_array: 'strings',
    pointer_array: 'strings',
    value: 'value',
    value_array: 'values',
    condition: 'condition',
    condition_array: 'condition_array',
    action_array: 'action_array',
}
export const readonlyRuleFields = ['id', 'revision', 'order_index', 'created_at', 'updated_at', 'legacy_name', 'source']
export function adaptSchema(raw: BackendRuleSchema): RuleSchema {
    if (![1,2].includes(raw.schema_version)) throw new Error('Unsupported rule schema version')
    const convert = (f: BackendField, readonly = false): RuleField => {
        const type = f.enum?.length ? 'enum' : backendRenderers[f.type]
        if (!type) throw new Error(`Unsupported renderer: ${f.type}`)
        if (!f.description?.trim() || !f.examples?.length) throw new Error(`Incomplete parameter documentation: ${f.name}`)
        if (f.enum?.some(option => !f.enum_help?.[option])) throw new Error(`Missing option documentation: ${f.name}`)
        const result: RuleField = {
            name:f.name, label:f.label, type, required:f.required, enum:f.enum, min:f.minimum,max:f.maximum,
            description:f.help ?? help(f.description,f.description), examples:f.examples,
            enum_help:f.enum_help,depends_on:f.depends_on,non_empty:f.non_empty,readonly,
        }
        if (f.default !== undefined && f.default !== null) result.default=f.default
        else if(type==='value')result.default=null
        return result
    }
    const caps = (values:BackendCapability[]):RuleCapability[] => values.filter(c=>!c.deprecated).map(c=>{
        if(!c.label || !c.description)throw new Error(`Incomplete capability documentation: ${c.id}`)
        return {id:c.id,label:c.label,phases:c.phases,description:help(c.description,c.description),fields:c.fields.map(f=>convert(f))}
    })
    const reference=raw.value_expressions.find(c=>c.id==='reference')!
    return {
        schema_version:raw.schema_version,phases:raw.phases,
        sources:['current','original','client','context','vars','item'], encodings:['value','json'],
        rule_fields:raw.rule_fields.map(f=>convert(f,readonlyRuleFields.includes(f.name))),
        conditions:caps(raw.conditions),actions:caps(raw.actions),
        condition_fields:[],value_fields:reference.fields.map(f=>convert(f)),
        value_expressions:caps(raw.value_expressions),context_fields:raw.context_fields.map(f=>convert(f)),examples:raw.examples,
    }
}
