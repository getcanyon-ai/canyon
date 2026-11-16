export default async function Page({ searchParams }: { searchParams: Promise<{ q: string }> }) {
  const { q } = await searchParams;
  const rows = await query(`SELECT * FROM t WHERE q = '${q}'`);
  return <div>{rows.length}</div>;
}
