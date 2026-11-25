var app = WebApplication.CreateBuilder(args).Build();
app.MapGet("/", () => $"dotnet {Environment.Version}\n");
app.Run();
