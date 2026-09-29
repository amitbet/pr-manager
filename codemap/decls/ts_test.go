package decls

import (
	"fmt"
	"strings"
	"testing"
)

func TestParseTSFile(t *testing.T) {
	src := `import React, { useState as useS } from 'react';
import * as api from './api';
import type { Foo } from '@/types';
export { bar } from './bar';
export * from './all';

// a comment with 'quotes' and { braces
export const URL = '/settings/api/v1/profiles';

export function useThing(x: number) {
  const [s] = useS(0);
  return api.load(` + "`${URL}/${x}`" + `);
}

const Inner = () => <div className="a">Don't {useThing(1)}</div>;

export default Inner;
`
	f := ParseTS("src/x.tsx", src)
	var names []string
	for _, d := range f.Decls {
		names = append(names, d.Name)
	}
	got := strings.Join(names, ",")
	if got != "URL,useThing,Inner,default" {
		t.Fatalf("decls = %s", got)
	}
	for _, d := range f.Decls {
		switch d.Name {
		case "URL":
			if !d.HasStr || d.StrVal != "/settings/api/v1/profiles" || !d.Exported {
				t.Errorf("URL decl = %+v", d)
			}
		case "useThing":
			if d.Line != 10 || d.EndLine != 13 {
				t.Errorf("useThing lines = %d-%d", d.Line, d.EndLine)
			}
		case "default":
			if d.AliasOf != "Inner" {
				t.Errorf("default alias = %q", d.AliasOf)
			}
		}
	}
	imps := map[string]string{}
	for _, im := range f.Imports {
		imps[im.Local] = im.Imported + "@" + im.Spec
	}
	for k, v := range map[string]string{"React": "default@react", "useS": "useState@react", "api": "*@./api", "Foo": "Foo@@/types"} {
		if imps[k] != v {
			t.Errorf("import %s = %q, want %q", k, imps[k], v)
		}
	}
	if len(f.Reexports) != 2 {
		t.Errorf("reexports = %+v", f.Reexports)
	}
}

func TestParseCommonJS(t *testing.T) {
	src := `'use strict';
const express = require('express');
const db = require('./db');
const { load, save: store } = require('../store');
const cfg = require('./config').settings;

function handler(req, res) {
  return store(load(req.id));
}

class Cache {}

exports.helper = function (x) { return x; };
module.exports.handler = handler;
module.exports = { handler, Cache, alias: helper2, inline() {} };
`
	f := ParseTS("lib/h.js", src)
	var ims []string
	for _, im := range f.Imports {
		ims = append(ims, im.Local+"="+im.Imported+"@"+im.Spec)
	}
	if got := strings.Join(ims, " "); got != "express=*@express db=*@./db load=load@../store store=save@../store cfg=settings@./config" {
		t.Errorf("imports = %s", got)
	}
	var names []string
	for _, d := range f.Decls {
		names = append(names, d.Name)
	}
	if got := strings.Join(names, ","); got != "handler,Cache,helper,<export>,<export>" {
		t.Errorf("decls = %s", got)
	}
	for exp, local := range map[string]string{"handler": "handler", "Cache": "Cache", "alias": "helper2"} {
		if f.LocalExp[exp] != local {
			t.Errorf("LocalExp[%s] = %q", exp, f.LocalExp[exp])
		}
	}
	if _, ok := f.LocalExp["inline"]; ok {
		t.Errorf("inline method is not a local export")
	}

	def := ParseTS("lib/d.js", "class A {}\nmodule.exports = A;\n")
	last := def.Decls[len(def.Decls)-1]
	if !last.IsDef || last.AliasOf != "A" {
		t.Errorf("module.exports = A -> %+v", last)
	}
}

func tsNames(f *TSFile) string {
	var out []string
	for _, d := range f.Decls {
		out = append(out, fmt.Sprintf("%s:%d-%d", d.Name, d.Line, d.EndLine))
	}
	return strings.Join(out, " ")
}

func TestTSStatementsEndDecls(t *testing.T) {
	src := `import { h } from './h';
function a() {
  return 1;
}
router.get('/x', h);
setup()
const b = 1
b.toString()
export const c =
  a()
@Component({
  selector: 'x',
})
export class D {}
type T = Map<string, number>
init()
`
	f := ParseTS("src/x.ts", src)
	if got := tsNames(f); got != "a:2-4 b:7-7 c:9-10 D:11-14 T:15-15" {
		t.Errorf("decls = %s", got)
	}
}

func TestTSClassMembers(t *testing.T) {
	src := `export class Svc extends Base implements I {
  private readonly repo: Repo;
  count = 0;
  static create(): Svc { return new Svc(); }
  constructor(repo: Repo) {
    super();
    this.repo = repo;
  }
  @HostListener('click')
  onClick(e) { this.run(e); }
  async run(x: number): Promise<void> {
    if (x) { await this.repo.save(x); }
  }
  load(a: string): void;
  load(a: any) {}
  get size() { return this.count; }
  set size(v) { this.count = v; }
  handle = (e: Event) => {
    this.run(1);
  };
  #secret() {}
  [Symbol.iterator]() {}
  cb: () => void = () => {};
  label: string = 'x';
}
class Other { m() {} }
`
	f := ParseTS("src/svc.ts", src)
	want := "Svc:1-25 Svc.create:4-4 Svc.constructor:5-8 Svc.onClick:9-10 Svc.run:11-13 Svc.load:14-15 Svc.size:16-17 Svc.handle:18-20 Svc.#secret:21-21 Svc.cb:23-23 Other:26-26 Other.m:26-26"
	if got := tsNames(f); got != want {
		t.Errorf("decls =\n%s\nwant\n%s", got, want)
	}
	for _, d := range f.Decls {
		if d.Name == "Svc.run" && (d.Owner != "Svc" || !d.Exported) {
			t.Errorf("run = %+v", d)
		}
		if d.Name == "Svc.#secret" && d.Exported {
			t.Errorf("#secret exported")
		}
	}
}

func TestTSJSX(t *testing.T) {
	src := `import { Button, Icon } from './ui';
const El = ({ items }) => (
  <div className="list" data-x='1'>
    Button text isn't code, Icon neither
    {items.map((it) => <Button key={it.id} onClick={() => go(it)}>{it.label}</Button>)}
    <ui.Icon name="x" />
    <>{/* comment */}</>
  </div>
);
function go(x) { return x < 1 && y > 2; }
const g = <T,>(x: T) => x;
`
	f := ParseTS("src/x.tsx", src)
	var ids []string
	for _, tk := range f.Toks {
		if tk.Kind == 'i' && tk.Line >= 3 && tk.Line <= 8 {
			ids = append(ids, fmt.Sprintf("%s@%d", tk.Text, tk.Line))
		}
	}
	if got := strings.Join(ids, " "); got != "items@5 map@5 it@5 Button@5 it@5 id@5 go@5 it@5 it@5 label@5 ui@6 Icon@6" {
		t.Errorf("idents = %s", got)
	}
	if got := tsNames(f); got != "El:2-9 go:10-10 g:11-11" {
		t.Errorf("decls = %s", got)
	}
	// In .ts, <T>x is a cast, not markup.
	ts := ParseTS("src/x.ts", "const a = <Foo>bar;\nconst b = 1;\n")
	if got := tsNames(ts); got != "a:1-1 b:2-2" {
		t.Errorf(".ts decls = %s", got)
	}
}

func TestTSTemplateHolesStayInDecl(t *testing.T) {
	src := "export const Styled = styled.div`\n  ${Mixin}\n  color: ${(p) => p.c};\n`\nconst next = 1\n"
	f := ParseTS("src/x.ts", src)
	if got := tsNames(f); got != "Styled:1-4 next:5-5" {
		t.Errorf("decls = %s", got)
	}
}

// Unterminated comments, regexes and templates must not run the lexer past
// the end of the source.
func TestTSLexTruncated(t *testing.T) {
	for _, src := range []string{
		"function f() { return <Foo /* todo",
		"const s = `${/*",
		"const s = `${/**",
		"const r = /a\\",
		"const x = <A extends /* ",
	} {
		ParseTS("src/x.tsx", src)
		ParseTS("src/x.ts", src)
	}
	sample := `import React from 'react';
/** Doc */
export const Row = <T,>({ items }: { items: T[] }) => (
  <Table<T> rows={items} /* note */ render={(r) => ` + "`${r.id /* id */}-${/x\\//.test(r.name)}`" + `} />
);
export class C { m() { return /[\]/]+/g.exec('a'); } }
`
	for i := 0; i <= len(sample); i++ {
		ParseTS("src/x.tsx", sample[:i])
		ParseTS("src/x.ts", sample[:i])
	}
}
