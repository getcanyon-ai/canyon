export async function POST(req: Request) {
  const { col } = await req.json();
  return Response.json(await knex.raw('SELECT ' + col + ' FROM t'));
}
