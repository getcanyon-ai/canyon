import { exec } from "child_process";
import { readFile } from "fs/promises";
export async function run(name: string, code: string) {
  exec("convert " + name);
  await readFile("/data/" + name);
  eval(code);
  return new Function(code);
}
